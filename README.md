# dump1090-exporter

[![CI](https://github.com/technothatch/dump1090-exporter/actions/workflows/ci.yml/badge.svg)](https://github.com/technothatch/dump1090-exporter/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://github.com/technothatch/dump1090-exporter/blob/main/LICENSE)
[![Latest Release](https://img.shields.io/github/v/release/technothatch/dump1090-exporter)](https://github.com/technothatch/dump1090-exporter/releases)
[![Issues](https://img.shields.io/github/issues/technothatch/dump1090-exporter)](https://github.com/technothatch/dump1090-exporter/issues)
[![PRs Welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg)](https://github.com/technothatch/dump1090-exporter/pulls)
[![Code size](https://img.shields.io/github/languages/code-size/technothatch/dump1090-exporter)](https://github.com/technothatch/dump1090-exporter)
[![Last commit](https://img.shields.io/github/last-commit/technothatch/dump1090-exporter)](https://github.com/technothatch/dump1090-exporter/commits/main)
[![Go](https://img.shields.io/badge/Go-1.25%2B-00ADD8.svg)](https://go.dev/doc/devel/release)
[![Coverage](https://img.shields.io/badge/coverage-93%25-brightgreen.svg)](https://github.com/technothatch/dump1090-exporter)

Prometheus exporter for the JSON documents served by
[dump1090-fa](https://github.com/flightaware/dump1090) and
[readsb](https://github.com/wiedehopf/readsb).

Both decoders already publish everything you need to monitor a receiver —
`aircraft.json`, `receiver.json` and `stats.json` — but Prometheus cannot
scrape them directly. This exporter polls those three documents, turns them
into properly typed counters, gauges and a histogram, and serves them at
`/metrics` with zero configuration. It handles the two things that make
decoder metrics annoying in practice: **decoder restarts** (cumulative
counters are converted to never-decreasing deltas) and **cardinality**
(aircraft are counted, never labelled — squawk is the only per-aircraft
label, bounded to the 4096 valid Mode A codes).

## Features

- **Drop-in HTTP endpoint** — `--web.listen-address :9702`,
  `--web.telemetry-path /metrics`, plus `/healthz` for liveness probes.
- **Background polling + on-demand fallback** — scrapes every `5s` by
  default; if a scrape request arrives after a missed poll (exporter restart,
  decoder outage), the handler fetches fresh data first, so the very first
  `/metrics` request already carries live values.
- **Restart-safe counters** — `dump1090_messages_total`,
  `dump1090_blocks_dropped_total` and `dump1090_cpu_milliseconds` survive
  decoder restarts without ever decreasing.
- **Bounded label cardinality** — never labelled by ICAO hex; squawk labels
  are validated against `^[0-7]{4}$` so a broken upstream cannot explode the
  time series index.
- **Tolerant parsing** — unknown and missing JSON keys are ignored; a
  decoder that omits a window or sub-object leaves the last known value in
  place instead of writing a fake zero.
- **Graceful shutdown** — SIGINT/SIGTERM stops the poll loop, drains in-flight
  scrapes with a 5s grace period, then exits `0`.

### Metrics

| Metric | Type | Meaning |
| --- | --- | --- |
| `dump1090_up` | gauge | `1` if the last scrape succeeded, `0` otherwise |
| `dump1090_messages_total` | counter | Messages decoded (`aircraft.json`) |
| `dump1090_messages_per_second` | gauge | Rate between the two most recent scrapes |
| `dump1090_aircraft` | gauge | Aircraft currently tracked |
| `dump1090_aircraft_with_position` | gauge | Tracked aircraft carrying lat/lon |
| `dump1090_aircraft_altitude_bucket` | histogram | Barometric altitude in feet, one observation per aircraft per scrape |
| `dump1090_aircraft_squawk{squawk=}` | gauge | Aircraft per Mode A code (series for vanished squawks are zeroed) |
| `dump1090_signal_dbfs{quantile=}` | gauge | RSSI quantiles (`0.5`, `0.9`, `0.99`) over the current fleet |
| `dump1090_blocks_dropped_total` | counter | Sample blocks dropped by the demodulator |
| `dump1090_cpu_milliseconds{stage=}` | counter | CPU time per stage: `demod`, `reader`, `background` |
| `dump1090_cpr_{local,global}_{ok,bad,skipped}` | gauge | CPR position decode outcomes |
| `dump1090_tracks_all`, `dump1090_tracks_single_message` | gauge | Track counts |
| `dump1090_receiver_info{version=}` | gauge | Decoder version, `1` for the current one, `0` for retired ones |
| `dump1090_receiver_refresh_seconds` | gauge | Decoder JSON refresh interval |
| `dump1090_scrape_duration_seconds` | gauge | Wall time of the last scrape |

## Install

```bash
go install github.com/technothatch/dump1090-exporter/cmd/dump1090-exporter@latest
```

Requires Go 1.25 or newer. Or build from source:

```bash
git clone https://github.com/technothatch/dump1090-exporter.git
cd dump1090-exporter
go build -o dump1090-exporter ./cmd/dump1090-exporter
```

## Quickstart

Point the exporter at your decoder (dump1090-fa and readsb both serve JSON
on port 8080):

```console
$ dump1090-exporter --url http://localhost:8080
time=2026-10-04T12:14:30.534-04:00 level=INFO msg=serving address=:9702 path=/metrics decoder=http://localhost:8080 interval=5s
```

Scrape it:

```console
$ curl -s localhost:9702/metrics
# HELP dump1090_aircraft Aircraft currently tracked in aircraft.json.
# TYPE dump1090_aircraft gauge
dump1090_aircraft 5
# HELP dump1090_aircraft_altitude Barometric altitude of tracked aircraft in feet, observed per aircraft per scrape; alt_baro "ground" counts as 0.
# TYPE dump1090_aircraft_altitude histogram
dump1090_aircraft_altitude_bucket{le="0"} 1
dump1090_aircraft_altitude_bucket{le="1000"} 1
dump1090_aircraft_altitude_bucket{le="2500"} 2
...
dump1090_aircraft_altitude_bucket{le="+Inf"} 4
dump1090_aircraft_altitude_sum 46200
dump1090_aircraft_altitude_count 4
# HELP dump1090_aircraft_squawk Aircraft currently broadcasting each Mode A squawk; squawks no longer present are reported as 0.
# TYPE dump1090_aircraft_squawk gauge
dump1090_aircraft_squawk{squawk="1200"} 1
dump1090_aircraft_squawk{squawk="7700"} 1
...
dump1090_blocks_dropped_total 42
dump1090_cpu_milliseconds{stage="demod"} 12345.6
dump1090_cpu_milliseconds{stage="reader"} 2345.6
dump1090_cpu_milliseconds{stage="background"} 345.6
dump1090_messages_per_second 0
dump1090_messages_total 1.00025e+06
dump1090_receiver_info{version="readsb 4.0.0"} 1
dump1090_receiver_refresh_seconds 1
dump1090_scrape_duration_seconds 0.1250283
dump1090_signal_dbfs{quantile="0.5"} -12.4
dump1090_signal_dbfs{quantile="0.9"} -3.2
dump1090_signal_dbfs{quantile="0.99"} -3.2
dump1090_tracks_all 42
dump1090_tracks_single_message 3
dump1090_up 1
```

Add it to Prometheus:

```yaml
scrape_configs:
  - job_name: dump1090
    static_configs:
      - targets: ["localhost:9702"]
```

Then alert on what matters:

```yaml
- alert: Dump1090Down
  expr: dump1090_up == 0
  for: 2m
  annotations:
    summary: "ADS-B decoder {{ $labels.instance }} is unreachable"
```

## Configuration reference

| Flag | Env | Meaning | Default |
| --- | --- | --- | --- |
| `--web.listen-address` | — | Address the metrics HTTP server binds | `:9702` |
| `--web.telemetry-path` | — | Path metrics are exposed on (must start with `/`) | `/metrics` |
| `--scrape-interval` | — | Background poll interval for the three JSON documents | `5s` |
| `--timeout` | — | HTTP timeout per decoder request | `5s` |
| `--url` | `DUMP1090_URL` | Decoder base URL, `scheme://host[:port]`, no path | `http://localhost:8080` |
| `-h`, `--help` | — | Print usage and exit | — |

The flag wins over the environment variable, which wins over the default.
Endpoints served on the listen address:

| Path | Purpose |
| --- | --- |
| `/metrics` | Prometheus exposition (scrapes the decoder on demand if the background poll is stale) |
| `/healthz` | Liveness probe, always `200 ok` while the process runs |

Invalid flags (non-positive interval, telemetry path without a leading `/`)
exit with status `2`; a decoder that cannot be reached does **not** stop the
exporter — it keeps serving the last known values with `dump1090_up 0`.

## How it works

```
  dump1090-fa / readsb        exporter                         Prometheus
  ────────────────────        ────────                         ──────────
  receiver.json ─┐
  stats.json  ───┼─ Client.Fetch ──▶ Document ──▶ Exporter.Update
  aircraft.json ─┘   (3 GETs)                     │
                                                  │  gauges  ← aircraft/receiver/stats
                                                  │  delta   ← cumulative counters
                                                  │  labels  ← squawk/stage/quantile only
                                                  ▼
                                    private prometheus.Registry
                                                  ▲
                                  promhttp.HandlerFor(registry)
                                        │                │
        GET /metrics ───────────────────┘                └── on-demand scrape
        (every 5s ticker also feeds Update)              when the last poll is stale
```

- `internal/dump1090` — typed, tolerant decoding of the three documents.
  Every field is optional; anything unknown is ignored.
- `internal/exporter` — one `Exporter` owns a private registry of real
  `prometheus` collectors. `Update` folds a `Document` in; `MarkUnreachable`
  flips `dump1090_up` to `0` while leaving every other value untouched, so
  charts keep their last known state instead of flapping to absent.
- `internal/exporter/delta.go` — `deltaTracker` turns source counters into
  per-scrape deltas: on a decoder restart the source value drops, and the
  whole new value counts as progress, so the exported counter never
  decreases (which Prometheus requires).
- `cmd/dump1090-exporter` — flags, the ticker, `/metrics` + `/healthz`, the
  on-demand fallback in front of the promhttp handler, and graceful shutdown
  driven by `signal.NotifyContext` (SIGINT/SIGTERM).

## Development

```bash
git clone https://github.com/technothatch/dump1090-exporter.git
cd dump1090-exporter

gofmt -l .          # must print nothing
go vet ./...
go test -race -coverprofile=coverage.out -covermode=atomic ./...
go tool cover -func=coverage.out
golangci-lint run   # optional locally; CI always runs it
go build ./...
```

The same commands are wrapped in a `Makefile` if you have `make`:

```bash
make lint     # gofmt + go vet + golangci-lint
make test     # go test -race with coverage summary
make build    # go build ./...
make check    # everything CI runs
```

The suite is table-driven throughout: fixture `stats.json`/`aircraft.json`/
`receiver.json` documents through to metric families, counter delta and rate
math including restarts, unknown/missing fields, squawk label lifecycle, and
HTTP handler tests asserting `200` plus the documented metric names —
31 tests (101 subtests) at 93% statement coverage.

## Related projects

- [dump1090-fa](https://github.com/flightaware/dump1090) — FlightAware's Mode S decoder
- [readsb](https://github.com/wiedehopf/readsb) — high-performance fork, JSON docs in-repo
- [tar1090](https://github.com/wiedehopf/tar1090) — web UI for readsb
- [graphs1090](https://github.com/wiedehopf/graphs1090) — long-term graphs for readsb
- [squawk-decoder](https://github.com/technothatch/squawk-decoder) — squawk/category/type decoder for dump1090 and readsb
- [dump1090-cli](https://github.com/technothatch/dump1090-cli) — pretty CLI for live ADS-B data

## License

MIT © technothatch. See [LICENSE](LICENSE).

---

## Related projects

Part of the [technothatch ADS-B toolkit](https://github.com/technothatch/technothatch#readme) - the full index with one-liners for every tool lives there.

- **Run & operate:** [dump1090-stack](https://github.com/technothatch/dump1090-stack) - full Compose stack; [dump1090-healthcheck](https://github.com/technothatch/dump1090-healthcheck) - decoder watchdog; [dump1090-alerts](https://github.com/technothatch/dump1090-alerts) - rule-based alerting
- **Metrics & data:** [dump1090-exporter](https://github.com/technothatch/dump1090-exporter) - Prometheus; [dump1090-influx](https://github.com/technothatch/dump1090-influx) - InfluxDB/Telegraf; [dump1090-grafana](https://github.com/technothatch/dump1090-grafana) - dashboards; [dump1090-recorder](https://github.com/technothatch/dump1090-recorder) - SQLite history; [dump1090-coverage](https://github.com/technothatch/dump1090-coverage) - heatmaps & reports
- **Feeds & networking:** [beast-relay](https://github.com/technothatch/beast-relay) - Beast fan-out; [dump1090-geofence](https://github.com/technothatch/dump1090-geofence) - region-filtered feeds; [dump1090-to-mqtt](https://github.com/technothatch/dump1090-to-mqtt) - MQTT/Home Assistant
- **Interfaces:** [dump1090-dashboard](https://github.com/technothatch/dump1090-dashboard) - web UI; [adsb-tui](https://github.com/technothatch/adsb-tui) - terminal radar; [dump1090-cli](https://github.com/technothatch/dump1090-cli) - aircraft.json CLI; [dump1090-sdk](https://github.com/technothatch/dump1090-sdk) - TypeScript client
- **Testing & decoding:** [dump1090-simulator](https://github.com/technothatch/dump1090-simulator) - synthetic traffic; [dump1090-replay](https://github.com/technothatch/dump1090-replay) - recorded history replay; [squawk-decoder](https://github.com/technothatch/squawk-decoder) - squawk/emitter/ICAO decoder
