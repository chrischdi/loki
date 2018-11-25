# Tempo: Like Prometheus, but for logs.

[![CircleCI](https://circleci.com/gh/grafana/tempo/tree/master.svg?style=svg&circle-token=618193e5787b2951c1ea3352ad5f254f4f52313d)](https://circleci.com/gh/grafana/tempo/tree/master) [Design doc](https://docs.google.com/document/d/11tjK_lvp1-SVsFZjgOTr1vV3-q6vBAsZYIQ5ZeYBkyM/edit)

Tempo is a horizontally-scalable, highly-available, multi-tenant, log aggregation
system inspired by Prometheus.  It is design to be very cost effective, as it does
not index the contents of the logs, but rather a set of labels for each log steam.

## Run it locally

Tempo can be run in a single host, no-dependencies mode using the following commands.

Tempo consists of 3 components; `tempo` is the main server, responsible for storing
logs and processing queries.  `promtail` is the agent, responsible for gather logs
and sending them to tempo and `grafana` as the UI.

To run tempo, use the following commands:

```
$ go build ./cmd/tempo
$ ./tempo -config.file=./docs/tempo-local-config.yaml
...
```

To run promtail, use the following commands:

```
$ go build ./cmd/promtail
$ ./promtail -config.file=./docs/promtail-local-config.yaml -positions.file=./positions.yaml -client.url=http://localhost/api/prom/push
...
```

Grafana is Tempo's UI, so you'll also want to run one of those:

```
$ docker run -ti -p 3000:3000 -e "GF_EXPLORE_ENABLED=true" grafana/grafana-dev:master-377eaa891c1eefdec9c83a2ee4dcf5c81665ab1f
```

In the Grafana UI (http://localhost:3000), loging with "admin"/"admin", add a new "Grafana Logging" datasource for `http://host.docker.internal:80`, then go to explore and enjoy!

## Usage Instructions

Tempo is running in the ops-tools1 cluster.  You can query logs from that cluster
using the following commands:

```
$ go get github.com/grafana/tempo/cmd/logcli
$ . $GOPATH/src/github.com/grafana/tempo/env # env vars inc. URL, username etc
$ logcli labels job
https://logs-dev-ops-tools1.grafana.net/api/prom/label/job/values
cortex-ops/consul
cortex-ops/cortex-gw
...
$ logcli query '{job="cortex-ops/consul"}'
https://logs-dev-ops-tools1.grafana.net/api/prom/query?query=%7Bjob%3D%22cortex-ops%2Fconsul%22%7D&limit=30&start=1529928228&end=1529931828&direction=backward&regexp=
Common labels: {job="cortex-ops/consul", namespace="cortex-ops"}
2018-06-25T12:52:09Z {instance="consul-8576459955-pl75w"} 2018/06/25 12:52:09 [INFO] raft: Snapshot to 475409 complete
2018-06-25T12:52:09Z {instance="consul-8576459955-pl75w"} 2018/06/25 12:52:09 [INFO] raft: Compacting logs from 456973 to 465169
```

The `logcli` command is temporary until we have Grafana integration. The URLs of
the requests are printed to help with integration work.

```
$ logcli help
usage: logcli [<flags>] <command> [<args> ...]

A command-line for tempo.

Flags:
  --help         Show context-sensitive help (also try --help-long and --help-man).
  --addr="https://log-us.grafana.net"
                 Server address.
  --username=""  Username for HTTP basic auth.
  --password=""  Password for HTTP basic auth.

Commands:
  help [<command>...]
    Show help.

  query [<flags>] <query> [<regex>]
    Run a LogQL query.

  labels <label>
    Find values for a given label.

$ logcli help query
usage: logcli query [<flags>] <query> [<regex>]

Run a LogQL query.

Flags:
  --help         Show context-sensitive help (also try --help-long and --help-man).
  --addr="https://log-us.grafana.net"
                 Server address.
  --username=""  Username for HTTP basic auth.
  --password=""  Password for HTTP basic auth.
  --limit=30     Limit on number of entries to print.
  --since=1h     Lookback window.
  --forward      Scan forwards through logs.

Args:
  <query>    eg '{foo="bar",baz="blip"}'
  [<regex>]
```
