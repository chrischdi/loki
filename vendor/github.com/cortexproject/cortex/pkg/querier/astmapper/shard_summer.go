package astmapper

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/promql"
)

const (
	// ShardLabel is a reserved label referencing a cortex shard
	ShardLabel = "__cortex_shard__"
	// ShardLabelFmt is the fmt of the ShardLabel key.
	ShardLabelFmt = "%d_of_%d"
)

var (
	// ShardLabelRE matches a value in ShardLabelFmt
	ShardLabelRE = regexp.MustCompile("^[0-9]+_of_[0-9]+$")
)

type squasher = func(...promql.Node) (promql.Expr, error)

type shardSummer struct {
	shards       int
	currentShard *int
	squash       squasher

	// Metrics.
	shardedQueries prometheus.Counter
}

// NewShardSummer instantiates an ASTMapper which will fan out sum queries by shard
func NewShardSummer(shards int, squasher squasher, registerer prometheus.Registerer) (ASTMapper, error) {
	if squasher == nil {
		return nil, errors.Errorf("squasher required and not passed")
	}

	return NewASTNodeMapper(&shardSummer{
		shards:       shards,
		squash:       squasher,
		currentShard: nil,
		shardedQueries: promauto.With(registerer).NewCounter(prometheus.CounterOpts{
			Namespace: "cortex",
			Name:      "frontend_sharded_queries_total",
			Help:      "Total number of sharded queries",
		}),
	}), nil
}

// CopyWithCurShard clones a shardSummer with a new current shard.
func (summer *shardSummer) CopyWithCurShard(curshard int) *shardSummer {
	s := *summer
	s.currentShard = &curshard
	return &s
}

// shardSummer expands a query AST by sharding and re-summing when possible
func (summer *shardSummer) MapNode(node promql.Node) (promql.Node, bool, error) {

	switch n := node.(type) {
	case *promql.AggregateExpr:
		if CanParallelize(n) && n.Op == promql.SUM {
			result, err := summer.shardSum(n)
			return result, true, err
		}

		return n, false, nil

	case *promql.VectorSelector:
		if summer.currentShard != nil {
			mapped, err := shardVectorSelector(*summer.currentShard, summer.shards, n)
			return mapped, true, err
		}
		return n, true, nil

	case *promql.MatrixSelector:
		if summer.currentShard != nil {
			mapped, err := shardMatrixSelector(*summer.currentShard, summer.shards, n)
			return mapped, true, err
		}
		return n, true, nil

	default:
		return n, false, nil
	}
}

// shardSum contains the logic for how we split/stitch legs of a parallelized sum query
func (summer *shardSummer) shardSum(expr *promql.AggregateExpr) (promql.Node, error) {

	parent, subSums, err := summer.splitSum(expr)
	if err != nil {
		return nil, err
	}

	combinedSums, err := summer.squash(subSums...)

	if err != nil {
		return nil, err
	}

	parent.Expr = combinedSums
	return parent, nil
}

// splitSum forms the parent and child legs of a parallel query
func (summer *shardSummer) splitSum(
	expr *promql.AggregateExpr,
) (
	parent *promql.AggregateExpr,
	children []promql.Node,
	err error,
) {
	parent = &promql.AggregateExpr{
		Op:    expr.Op,
		Param: expr.Param,
	}
	var mkChild func(sharded *promql.AggregateExpr) promql.Expr

	if expr.Without {
		/*
			parallelizing a sum using without(foo) is representable naively as
			sum without(foo) (
			  sum without(__cortex_shard__) (rate(bar1{__cortex_shard__="0_of_2",baz="blip"}[1m])) or
			  sum without(__cortex_shard__) (rate(bar1{__cortex_shard__="1_of_2",baz="blip"}[1m]))
			)
			or (more optimized):
			sum without(__cortex_shard__) (
			  sum without(foo) (rate(bar1{__cortex_shard__="0_of_2",baz="blip"}[1m])) or
			  sum without(foo) (rate(bar1{__cortex_shard__="1_of_2",baz="blip"}[1m]))
			)

		*/
		parent.Grouping = []string{ShardLabel}
		parent.Without = true
		mkChild = func(sharded *promql.AggregateExpr) promql.Expr {
			sharded.Grouping = expr.Grouping
			sharded.Without = true
			return sharded
		}
	} else if len(expr.Grouping) > 0 {
		/*
			parallelizing a sum using by(foo) is representable as
			sum by(foo) (
			  sum by(foo, __cortex_shard__) (rate(bar1{__cortex_shard__="0_of_2",baz="blip"}[1m])) or
			  sum by(foo, __cortex_shard__) (rate(bar1{__cortex_shard__="1_of_2",baz="blip"}[1m]))
			)
		*/
		parent.Grouping = expr.Grouping
		mkChild = func(sharded *promql.AggregateExpr) promql.Expr {
			groups := make([]string, 0, len(expr.Grouping)+1)
			groups = append(groups, expr.Grouping...)
			groups = append(groups, ShardLabel)
			sharded.Grouping = groups
			return sharded
		}
	} else {
		/*
			parallelizing a non-parameterized sum is representable as
			sum(
			  sum without(__cortex_shard__) (rate(bar1{__cortex_shard__="0_of_2",baz="blip"}[1m])) or
			  sum without(__cortex_shard__) (rate(bar1{__cortex_shard__="1_of_2",baz="blip"}[1m]))
			)
			or (more optimized):
			sum without(__cortex_shard__) (
			  sum by(__cortex_shard__) (rate(bar1{__cortex_shard__="0_of_2",baz="blip"}[1m])) or
			  sum by(__cortex_shard__) (rate(bar1{__cortex_shard__="1_of_2",baz="blip"}[1m]))
			)
		*/
		parent.Grouping = []string{ShardLabel}
		parent.Without = true
		mkChild = func(sharded *promql.AggregateExpr) promql.Expr {
			sharded.Grouping = []string{ShardLabel}
			return sharded
		}
	}

	// iterate across shardFactor to create children
	for i := 0; i < summer.shards; i++ {
		cloned, err := CloneNode(expr.Expr)
		if err != nil {
			return parent, children, err
		}

		subSummer := NewASTNodeMapper(summer.CopyWithCurShard(i))
		sharded, err := subSummer.Map(cloned)
		if err != nil {
			return parent, children, err
		}

		subSum := mkChild(&promql.AggregateExpr{
			Op:   expr.Op,
			Expr: sharded.(promql.Expr),
		})

		children = append(children,
			subSum,
		)
	}

	summer.shardedQueries.Add(float64(summer.shards))

	return parent, children, nil
}

func shardVectorSelector(curshard, shards int, selector *promql.VectorSelector) (promql.Node, error) {
	shardMatcher, err := labels.NewMatcher(labels.MatchEqual, ShardLabel, fmt.Sprintf(ShardLabelFmt, curshard, shards))
	if err != nil {
		return nil, err
	}

	return &promql.VectorSelector{
		Name:   selector.Name,
		Offset: selector.Offset,
		LabelMatchers: append(
			[]*labels.Matcher{shardMatcher},
			selector.LabelMatchers...,
		),
	}, nil
}

func shardMatrixSelector(curshard, shards int, selector *promql.MatrixSelector) (promql.Node, error) {
	shardMatcher, err := labels.NewMatcher(labels.MatchEqual, ShardLabel, fmt.Sprintf(ShardLabelFmt, curshard, shards))
	if err != nil {
		return nil, err
	}

	if vs, ok := selector.VectorSelector.(*promql.VectorSelector); ok {
		return &promql.MatrixSelector{
			VectorSelector: &promql.VectorSelector{
				Name:   vs.Name,
				Offset: vs.Offset,
				LabelMatchers: append(
					[]*labels.Matcher{shardMatcher},
					vs.LabelMatchers...,
				),
				PosRange: vs.PosRange,
			},
			Range:  selector.Range,
			EndPos: selector.EndPos,
		}, nil
	}

	return nil, fmt.Errorf("invalid selector type: %T", selector.VectorSelector)
}

// ParseShard will extract the shard information encoded in ShardLabelFmt
func ParseShard(input string) (parsed ShardAnnotation, err error) {
	if !ShardLabelRE.MatchString(input) {
		return parsed, errors.Errorf("Invalid ShardLabel value: [%s]", input)
	}

	matches := strings.Split(input, "_")
	x, err := strconv.Atoi(matches[0])
	if err != nil {
		return parsed, err
	}
	of, err := strconv.Atoi(matches[2])
	if err != nil {
		return parsed, err
	}

	if x >= of {
		return parsed, errors.Errorf("Shards out of bounds: [%d] >= [%d]", x, of)
	}
	return ShardAnnotation{
		Shard: x,
		Of:    of,
	}, err
}

// ShardAnnotation is a convenience struct which holds data from a parsed shard label
type ShardAnnotation struct {
	Shard int
	Of    int
}

// String encodes a shardAnnotation into a label value
func (shard ShardAnnotation) String() string {
	return fmt.Sprintf(ShardLabelFmt, shard.Shard, shard.Of)
}

// Label generates the ShardAnnotation as a label
func (shard ShardAnnotation) Label() labels.Label {
	return labels.Label{
		Name:  ShardLabel,
		Value: shard.String(),
	}
}

// ShardFromMatchers extracts a ShardAnnotation and the index it was pulled from in the matcher list
func ShardFromMatchers(matchers []*labels.Matcher) (shard *ShardAnnotation, idx int, err error) {
	for i, matcher := range matchers {
		if matcher.Name == ShardLabel && matcher.Type == labels.MatchEqual {
			shard, err := ParseShard(matcher.Value)
			if err != nil {
				return nil, i, err
			}
			return &shard, i, nil
		}
	}
	return nil, 0, nil
}
