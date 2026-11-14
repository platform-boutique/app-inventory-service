# inventory-service

## Purpose and place in the shop

A gRPC service that owns the shop's stock: how many pieces of each product can still be sold, and the reservations (holds) that checkout places while an order is being paid and shipped. It owns its own Postgres database and creates the schema and the seed stock with embedded migrations at start-up. Demo grade (see [Known limits](#known-limits)): no restock API, no admin, no caller authentication. It calls no other service.

Context diagram and the main flow as sequence diagram: [`docs/architecture.md`](docs/architecture.md).

| | |
|---|---|
| Language / runtime | Go 1.26 (`go.mod`: `go 1.26.0`, `toolchain go1.26.11`), static binary, `CGO_ENABLED=0` |
| Image | built locally only: `platform-demo/inventory-service:local` (`Dockerfile`, user `10001:10001`) |
| Ports | gRPC `8087` (plaintext), HTTP `9090` (`/metrics`, scraped by Alloy; pprof only on its own `PPROF_ADDR` listener) |
| Health | `grpc.health.v1.Health/Check` on `8087`, `NOT_SERVING` until the migrations and the seed are done and whenever the database does not answer a ping (every 5 s) |
| State | Postgres (`stock`, `reservations`, `reservation_items`) via `DATABASE_URL` |
| Depends on | its own Postgres (required); OTLP collector and Pyroscope (optional) |
| Called by | checkout-service (`Reserve`, `Commit`, `Release`) and the frontend (`GetStock`); wiring seen in `local/compose.yaml` (`INVENTORY_SERVICE_ADDR` on both), not re-verified in their code |
| gRPC reflection | enabled |

## Status

No CI pipeline, GitHub repository, Helm chart or Backstage registration exists yet; the service is built and run locally only. Status badges are added when the repo has a pipeline.

## Quick start

In the platform-demo workspace (this repo is built from `apps/inventory-service` by `local/compose.yaml`; the compose commands were not run for this README):

```bash
local/up.sh                      # builds all images and starts the shop, http://localhost:8080
SKIP_BUILD=1 local/up.sh         # reuse images
local/rebuild.sh inventory-service  # rebuild and recreate only this service
```

Run the tests (run on 2026-11-13, see Develop):

```bash
GOTOOLCHAIN=auto go vet ./... && GOTOOLCHAIN=auto go test -race -count=1 ./...
```

## API

Package `hipstershop.inventory`, service `InventoryService` ([`proto/inventory.proto`](proto/inventory.proto)). Full rules, state machine, error codes and `grpcurl` examples: [`docs/api.md`](docs/api.md).

| Method | Request | Behaviour | Error codes |
|---|---|---|---|
| `GetStock` | `product_ids` (1-100) | Levels (`available` = what can still be reserved now) of the known products, in request order; unknown ids are missing from the answer | `INVALID_ARGUMENT`, `UNAVAILABLE`, `INTERNAL` |
| `Reserve` | `order_id`, `items` (1-100 lines, `quantity` 1-1000) | All or nothing in one transaction: takes the quantities and answers `reserved = true`, or `reserved = false` with every short product in `shortages` and nothing held. Replay with the same lines is `reserved = true` | `INVALID_ARGUMENT`, `ALREADY_EXISTS` (same order id, other lines), `FAILED_PRECONDITION` (released or expired), `UNAVAILABLE`, `INTERNAL` |
| `Commit` | `order_id` | RESERVED to COMMITTED (stock stays taken). Idempotent | `NOT_FOUND`, `FAILED_PRECONDITION`, `INVALID_ARGUMENT`, `UNAVAILABLE`, `INTERNAL` |
| `Release` | `order_id` | RESERVED to RELEASED: quantities go back to `available`. Idempotent; an unknown order id is `OK` | `FAILED_PRECONDITION` (already COMMITTED), `INVALID_ARGUMENT`, `UNAVAILABLE`, `INTERNAL` |

A hold that is not committed expires after `RESERVATION_TTL`; its stock is returned lazily by the next call that mentions its products and by a background sweep. All handlers answer `UNAVAILABLE` until the migrations have run; a handler panic becomes `INTERNAL`.

## Configuration

Every variable read by the code (`grep -rn 'Getenv\|getenv'` over `cmd/` and `internal/`, checked 2026-11-13). All configuration is by environment; there is no config file.

| Variable | Default | Required | Meaning |
|---|---|---|---|
| `DATABASE_URL` | none | **yes** | Postgres URL, e.g. `postgres://inventory:inventory@inventory-db:5432/inventory?sslmode=disable`. The process logs an error and exits 1 if it is unset. A 5 s connect timeout is added unless the URL sets one |
| `PORT` | `8087` | no | gRPC listen port. The `healthcheck` subcommand reads it too |
| `RESERVATION_TTL` | `15m` | no | How long a RESERVED hold lives (Go duration). Invalid or non-positive: the process exits |
| `SWEEP_INTERVAL` | `1m` | no | How often the background sweep looks for expired holds (Go duration; added so tests and demos need not wait a minute). Invalid or non-positive: the process exits |
| `HOSTNAME` | OS hostname | no | Used as `service.instance.id` (the pod name in Kubernetes) |
| `METRICS_PORT` | `9090` | no | Port of the Prometheus `/metrics` endpoint (scraped by Alloy). A value that is not a port falls back to `9090` with a warning |
| `PPROF_ADDR` | unset (no pprof) | no | `host:port` of a separate pprof listener, e.g. `127.0.0.1:6060`. Off unless set; never expose it. pprof is never served on the metrics port |
| `LOG_LEVEL` | `info` | no | `debug`, `info`, `warn` or `error`; anything else falls back to `info` with a warning |
| `SERVICE_VERSION` | build info, else `dev` | no | `service.version`, the `version` log field and the `build_info` label. Fallbacks in order: ldflags `-X <module>/internal/obs.Version=...` (and `.Commit=`, `.BuildTime=`), the VCS revision Go embeds in the binary (12 characters, `-dirty` if modified), `dev` |
| `DEPLOYMENT_ENVIRONMENT` | `local` | no | `deployment.environment` on every signal and the Pyroscope tag `environment` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | unset (telemetry off) | no | OTLP/**gRPC** endpoint for **traces, metrics and logs**, e.g. `http://alloy:4317`. Telemetry is on only if this is a valid absolute http(s) URL; empty or malformed means off (one warning, never a crash). The legacy `COLLECTOR_SERVICE_ADDR` and `ENABLE_TRACING` are no longer read |
| `OTEL_SDK_DISABLED` | unset | no | `true` (trimmed, case-insensitive) forces everything off: OTLP export and profiling, even if an endpoint or `PYROSCOPE_ADDR` is set |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `grpc` | no | Documented knob; only gRPC is implemented, the value is not read |
| `OTEL_TRACES_SAMPLER`, `OTEL_TRACES_SAMPLER_ARG` | SDK default (`parentbased_always_on`) | no | Standard OpenTelemetry sampler variables, read by the SDK (tested for `always_on` and `always_off`) |
| `OTEL_RESOURCE_ATTRIBUTES` | unset | no | Extra resource attributes (`k=v,...`); applied after the defaults, so they can override them |
| `OTEL_SERVICE_NAME` | `inventory-service` | no | `service.name`; keep the default, dashboards depend on it |
| `OTEL_METRIC_EXPORT_INTERVAL` | `15000` | no | Milliseconds between metric pushes (also accepts durations such as `15s`) |
| `PYROSCOPE_ADDR` | unset (profiling off) | no | Pyroscope URL (SDK push protocol, not OTLP). The only switch: no default address, `ENABLE_PROFILING=1` alone starts nothing |
| `ENABLE_PROFILING` | unset | no | `0` forces profiling off even with `PYROSCOPE_ADDR`; any other value changes nothing |

The OTLP exporters of the OpenTelemetry Go SDK also read their own standard `OTEL_EXPORTER_OTLP_*` variables (headers, TLS, timeout); they are not read by this service's code and were not tested. The sampler is the SDK's (`OTEL_TRACES_SAMPLER`, default `parentbased_always_on`).

Test-only variables (not read by the service): `TEST_DATABASE_URL` for the integration tests, `CATALOG_SEED` for `TestSeedMatchesCatalog` (the Python tool takes the path as an argument instead).

Start-up: the gRPC server listens at once (health `NOT_SERVING`); a background loop runs the goose migrations (under goose's Postgres advisory lock) and the seed, retrying every 2 s until the database is reachable, then starts the sweep.

## Observability

| Signal | Implementation | Destination |
|---|---|---|
| **Traces** | OpenTelemetry (`otelgrpc` handlers), OTLP/gRPC exporter (batch processor, queue bounded at 2048), W3C TraceContext + Baggage propagated to every downstream call. Sampler from `OTEL_TRACES_SAMPLER` (SDK default `parentbased_always_on`) | Alloy `:4317` -> Tempo |
| **Metrics** | Prometheus default registry: `grpc_server_*` (go-grpc-prometheus with handling-time histogram), Go runtime and process metrics. Served as Prometheus text on `:9090/metrics` (`METRICS_PORT`) and, because the same registry is bridged into OpenTelemetry, also pushed over OTLP every 15 s (metric names identical). Plus `build_info{service,version,commit} 1` | Alloy scrapes `:9090` -> Mimir |
| **Logs** | `logrus`, one JSON object per line on stdout, plus an OTLP/gRPC hook (`otellogrus`) | stdout -> Alloy -> Loki; OTLP -> Alloy -> Loki |
| **Profiles** | Pyroscope Go SDK (CPU, alloc/in-use objects and space, goroutines, mutex, block), only with `PYROSCOPE_ADDR`; tags `service_name`, `version`, `environment` | Pyroscope |

**Telemetry contract** (the code is `internal/obs`, the same package in every Go service): telemetry is on only if `OTEL_EXPORTER_OTLP_ENDPOINT` is a valid absolute http(s) URL; `OTEL_SDK_DISABLED=true` forces everything off (profiling too); profiling only with `PYROSCOPE_ADDR`. Start never waits for the collector (the exporters connect in the background) and on SIGTERM the flush is cut off after 4 s, so a blackholed collector cannot hold the process (unit-tested with a listener that never answers: start and shutdown both return within the bound).

**Resource attributes** on every signal: `service.name` (`OTEL_SERVICE_NAME`, default `inventory-service`), `service.version` (`SERVICE_VERSION`, else build info, else `dev`), `service.namespace=everly`, `deployment.environment` (`DEPLOYMENT_ENVIRONMENT`, default `local`), `service.instance.id` (hostname). `OTEL_RESOURCE_ATTRIBUTES` is applied last.

**Log shape** (one shape for all services): `timestamp` (RFC 3339, UTC, milliseconds), `severity` (`debug`, `info`, `warning`, `error`), `message`, `service`, `version`, `trace_id` and `span_id` (hex, only while a valid span is active, never empty or all zeros), `request_id` (when the caller sent one), `error` (string, on errors), plus the call's own fields (for example `order_id`). Level from `LOG_LEVEL`. `trace_id`/`span_id` come from the context of the log call: handlers and clients log with `log.WithContext(ctx)`, the formatter reads the span from it. Calls without a context (startup, background work) have no ids. No card data, tokens or e-mail addresses are logged.

**Startup and version**: the first line is `message: "starting"` with `version`, `commit` and `environment`; `version`/`commit` come from `SERVICE_VERSION`, ldflags `-X <module>/internal/obs.Version=` / `.Commit=`, or the VCS data Go embeds (no Dockerfile change was made: without any of them a container image reports `version: "dev"`, `commit: "unknown"`). Panics are logged as one JSON line (`message: "panic"`, `error`) before the process dies; a startup error is one JSON error line, exit 1.

**pprof** is off. `PPROF_ADDR=127.0.0.1:6060` starts a separate listener; it is never on the metrics port.

**Graceful shutdown** (SIGTERM or SIGINT): health goes `NOT_SERVING`, `GracefulStop` with a 5 s cap (then `Stop`), the sweep stops, then the telemetry flush runs, cut off after 4 s, the pool is closed: the process exits within 9 s even when the collector is blackholed. Run against a listener that never answers (OTLP and Pyroscope both pointed at it, database unreachable) the binary exited 0 about 4 s after SIGTERM (smoke run, not part of the unit tests; with a reachable database not re-measured).

**Not there yet** (standards section 6): spans for Postgres calls; `build_info` is there, but DB pool stats, business counters (reservations by outcome, shortages, expired holds released) and stock levels are not. A handler logs the order id and counts only (`message: "reserve"` with `order_id`, `lines`, `reserved`, `shortages`; `commit`/`release` with `order_id`; `sweep released 3 expired reservation(s)`); product ids and quantities are never logged (a unit and an integration test check that).

Example query (not run against a live backend): `sum by (grpc_method, grpc_code) (rate(grpc_server_handled_total{grpc_service="hipstershop.inventory.InventoryService"}[5m]))`.

## Data

Postgres, database owned by this service. `stock(product_id pk, available int check >= 0)`, `reservations(order_id pk, status RESERVED|COMMITTED|RELEASED, created_at, updated_at, expires_at)` (partial index on `expires_at` for RESERVED rows) and `reservation_items(order_id, product_id, quantity 1-1000, pk (order_id, product_id))` with foreign keys to both. Check constraints repeat the id lengths. Invariant (checked by the integration tests after every race): for each product, `available + quantities of RESERVED and COMMITTED reservations = initial stock`, and `available` is never negative. How a reservation is made safe (conditional decrement, ordered row locks, per-order advisory lock): [`docs/architecture.md`](docs/architecture.md).

Migrations: [goose](https://github.com/pressly/goose) SQL files in `internal/inventory/migrations/`, embedded in the binary and applied at start-up under goose's advisory lock, forward-only (`00001_create_inventory.sql`).

Seed: `data/stock.json` (embedded, 117 products) holds `product_id -> available` copied from the `stock` field of the catalog seed `apps/product-catalog-service/data/seed/products.json`. At start the service inserts every product that has no stock row yet (`INSERT ... ON CONFLICT DO NOTHING`); it never changes an existing row, so a restart or a second replica never resets live stock, and a number changed in the catalog later does **not** change the stock of a product already in the database.

```bash
python3 tools/sync_stock.py [products.json]            # rewrites data/stock.json from the catalog seed (default: ../product-catalog-service/data/seed/products.json relative to the repo)
python3 tools/sync_stock.py --check [products.json]    # exit 0 when it matches, 1 with the differences, 2 when the catalog file is missing
CATALOG_SEED=/path/to/products.json go test ./internal/inventory -run TestSeedMatchesCatalog -v
```

`TestSeedMatchesCatalog` fails when the copy has drifted; it is skipped, with a message saying that drift was NOT checked, when the catalog file cannot be found (default: the sibling checkout, or `CATALOG_SEED`). Run on 2026-11-13 against `apps/product-catalog-service/data/seed/products.json` (checked-out branch `feat/catalog-migrations`): the test passed and `python3 tools/sync_stock.py --check <that file>` printed `ok: 117 products ... matches the catalog` (exit 0). The plain `go test ./...` run in the worktree skips this test because the worktree is not next to `product-catalog-service`.

## Develop

Requirements: Go 1.26 (`GOTOOLCHAIN=auto` fetches it), Docker for integration tests, [`grpcurl`](https://github.com/fullstorydev/grpcurl), Python 3 for the sync tool. Layout: `cmd/server/` (entry point, config, telemetry, health), `internal/inventory/` (service, store, validation, migrations, seed), `data/` (embedded seed), `tools/sync_stock.py`, `proto/`, `tests/integration/`.

| Step | Command | Result on 2026-11-13 |
|---|---|---|
| Vet + unit tests | `go vet ./... && go test -race -count=1 ./...` | run, passed |
| Format | `gofmt -l .` | run, no files listed |
| Lint | `golangci-lint run ./...` (config `.golangci.yaml`, v2 default linters) | run with golangci-lint v2.11.4, 0 issues; also with `--build-tags integration`, 0 issues. CI pins v2.14.0: not run |
| Integration (Postgres) | see below | not run today (earlier runs by the author are in `docs/development.md`) |
| Vulnerability / security scan | `govulncheck ./...`, `gosec -exclude-generated ./...` | not run today |
| Image | `docker build -t platform-demo/inventory-service:local .` | not run today |
| Docs | `mkdocs build --strict` (mkdocs 1.6.1 + mkdocs-techdocs-core 1.7.1, Python 3.12 venv made with uv) | run, passed |

Integration tests (build tag `integration`, run on an injected clock, each test creates and drops its own database):

```bash
docker run -d --rm --name inv-test-pg --memory 256m -e POSTGRES_USER=inv -e POSTGRES_PASSWORD=inv \
  -p 127.0.0.1:55437:5432 postgres:17-alpine
TEST_DATABASE_URL='postgres://inv:inv@127.0.0.1:55437/postgres?sslmode=disable' \
  go test -race -tags integration ./tests/integration/
docker rm -f inv-test-pg
```

Regenerate the Go code after editing the proto: `protoc -I proto --go_out=proto --go_opt=paths=source_relative --go-grpc_out=proto --go-grpc_opt=paths=source_relative proto/inventory.proto` (protoc 3.21.12, protoc-gen-go 1.36.11, protoc-gen-go-grpc 1.6.1; not run today). Proto changes are additive only, server first.

## Operate

- **Health:** `grpc.health.v1` on the service port, for the empty service name and `hipstershop.inventory.InventoryService`. `NOT_SERVING` until the migrations and the seed are done, and again whenever the Postgres ping fails (every 5 s); handlers answer `UNAVAILABLE` meanwhile. The image `HEALTHCHECK` runs `/app/inventory-service healthcheck` (exit 0 only on `SERVING`).
- **Shutdown:** on SIGTERM/SIGINT health goes `NOT_SERVING`, `GracefulStop` with a 5 s cap (then `Stop`), the sweep stops, the pool is closed, telemetry is flushed (cut off after 4 s), the metrics listener stops, exit 0. In total at most 9 s (5 s drain + 4 s flush) even with a blackholed collector.
- **Scaling:** stateless except for Postgres, several replicas are possible: the sweep runs in every replica (harmless, the release is conditional on the state under row locks), migrations and seed take advisory locks. With no replica running, holds are only released lazily. Several replicas against one database were not run (the tests use several `Service` values on one database, not several processes).
- **Image:** Alpine runtime, user `10001:10001`, `EXPOSE 8087 9090`, about 12 MB when last built.
- Failure behaviour and runbook: [`docs/operations.md`](docs/operations.md).

## Known limits

- The sweep is per process; without a running replica holds are only released lazily. A lazy release by `GetStock`/`Reserve` handles at most 100 expired holds per call, the sweep (200 per batch, repeated) takes the rest.
- No restock API, no admin, no way to change stock at runtime. New products appear after a restart with a newer seed; changed numbers for existing products do not.
- No caller authentication; plain gRPC without TLS (in the local stack it is on the internal network only).
- A `Commit` that arrives after `expires_at` is refused even if the sweep has not run yet (the stock is returned in that call).
- Expiry follows the wall clock of the process: no protection against clock skew between replicas or clock steps. With the default 15 minutes this is harmless; with tiny TTLs it is not (on the developer's WSL2 host about 1 hold in 100 got an `expires_at` far from the intended one with a 100 ms TTL). The stock books stay correct either way. Details: [`docs/operations.md`](docs/operations.md).
- Reserved quantities are not tied to a price or a user.
- No spans for database calls, no DB pool stats or business counters. The version/commit is logged at start and in `build_info`, but the image build sets no ldflags, so a container reports `dev` unless `SERVICE_VERSION` is set.
- Not run: OTLP exporters against a real collector and Pyroscope, arm64 build, `govulncheck`, several replicas, any load or timing measurement beyond the 200-request oversell test.

## License

Apache License 2.0, see [LICENSE](LICENSE).
