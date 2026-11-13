# Configuration

Every variable read by the code (`grep -rn 'Getenv\|getenv'` over `cmd/` and `internal/`, checked 2026-11-13). All configuration is by environment; there is no config file.

| Variable | Default | Required | Meaning |
|---|---|---|---|
| `DATABASE_URL` | none | **yes** | Postgres URL, e.g. `postgres://inventory:inventory@inventory-db:5432/inventory?sslmode=disable`. The process logs an error and exits 1 if it is unset. A 5 s connect timeout is added unless the URL sets one |
| `PORT` | `8087` | no | gRPC listen port. The `healthcheck` subcommand reads it too |
| `METRICS_PORT` | `9090` | no | HTTP port for `/metrics` (and pprof when profiling is on) |
| `RESERVATION_TTL` | `15m` | no | How long a RESERVED hold lives (Go duration). Invalid or non-positive: the process exits |
| `SWEEP_INTERVAL` | `1m` | no | How often the background sweep looks for expired holds (Go duration; added so tests and demos need not wait a minute). Invalid or non-positive: the process exits |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | unset (telemetry off) | no | OTLP/**gRPC** endpoint URL with scheme for traces, metrics and logs |
| `OTEL_SDK_DISABLED` | unset | no | `true` forces telemetry off even with an endpoint |
| `OTEL_SERVICE_NAME` | `inventory-service` | no | `service.name` resource attribute |
| `OTEL_METRIC_EXPORT_INTERVAL` | `15000` | no | Metric push interval: a number of milliseconds, or a Go duration (`15s`). Invalid values fall back to 15 s |
| `PYROSCOPE_ADDR` | unset (profiling off) | no | Pyroscope server URL. Profiling (push and pprof endpoints) is on only if this is set |
| `ENABLE_PROFILING` | unset | no | `0` forces profiling off even with `PYROSCOPE_ADDR` |
| `ENABLE_TRACING` | unset | no | Legacy: `0` forces telemetry off |
| `COLLECTOR_SERVICE_ADDR` | unset | no | **Legacy, to be removed** (owner decision 2026-11-13): `host:port` of a collector; turns telemetry on without `OTEL_EXPORTER_OTLP_ENDPOINT` |
| `HOSTNAME` | OS hostname | no | Used as `service.instance.id` (the pod name in Kubernetes) |
| `OTEL_RESOURCE_ATTRIBUTES` | unset | no | Read by the OTel SDK resource detector (`resource.WithFromEnv`), not by this code directly |

The OTLP exporters of the OpenTelemetry Go SDK also read their own standard `OTEL_EXPORTER_OTLP_*` variables (headers, TLS, timeout); they are not read by this service's code and were not tested. `OTEL_TRACES_SAMPLER` is **not** honoured: the sampler is fixed to always-on (`otlp.go`). `LOG_LEVEL` does not exist: the log level is logrus' default (info).

Test-only variables (not read by the service): `TEST_DATABASE_URL` for the integration tests, `CATALOG_SEED` for `TestSeedMatchesCatalog` and `tools/sync_stock.py`.

Start-up: the gRPC server listens at once (health `NOT_SERVING`); a background loop runs the goose migrations (under goose's Postgres advisory lock) and the seed, retrying every 2 s until the database is reachable, then starts the sweep.
