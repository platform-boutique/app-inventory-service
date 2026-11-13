# inventory-service

## Purpose and place in the shop

A gRPC service that owns the shop's stock: how many pieces of each product can still be sold, and the reservations (holds) that checkout places while an order is being paid and shipped. It owns its own Postgres database and creates the schema and the seed stock with embedded migrations at start-up. Demo grade (see [Known limits](#known-limits)): no restock API, no admin, no caller authentication. It calls no other service.

Context diagram and the main flow as sequence diagram: [`docs/architecture.md`](docs/architecture.md).

| | |
|---|---|
| Language / runtime | Go 1.26 (`go.mod`: `go 1.26.0`, `toolchain go1.26.10`), static binary, `CGO_ENABLED=0` |
| Image | built locally only: `platform-demo/inventory-service:local` (`Dockerfile`, user `10001:10001`) |
| Ports | gRPC `8087` (plaintext), HTTP `9090` (`/metrics`, pprof only with profiling) |
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

## Observability

What the service emits today (code read 2026-11-13; the compose observability profile was not run for this README):

| Signal | Today | Not there yet (standards section 6) |
|---|---|---|
| Traces | OTLP/gRPC, only when `OTEL_EXPORTER_OTLP_ENDPOINT` is set. A server span per inbound gRPC call (`otelgrpc`), W3C `traceparent` + `baggage` propagation, sampler always-on | spans for Postgres calls |
| Metrics | Prometheus on `:9090/metrics`: `grpc_server_*` (go-grpc-prometheus with handling-time histogram), Go runtime and process metrics. The same registry is pushed over OTLP when the endpoint is set | `build_info`, DB pool stats, business counters (reservations by outcome, shortages, expired holds released), stock levels |
| Logs | JSON on stdout, one object per line: `timestamp` (RFC3339Nano), `severity`, `message` plus fields. Mirrored over OTLP when the endpoint is set. A handler logs the order id and counts only (`reserve order=o-1 lines=2 reserved=false shortages=1`, `commit order=o-1`, `sweep released 3 expired reservation(s)`); product ids and quantities are never logged (a unit and an integration test check that). | `trace_id` / `span_id` on the stdout line, `service`, `version`, `LOG_LEVEL` |
| Profiles | Pyroscope push (CPU, alloc objects/space, inuse objects/space) and pprof on the metrics port, only with `PYROSCOPE_ADDR` | goroutine / mutex / block profiles, tags `version`, `environment` |

Resource attributes today: `service.name`, `service.instance.id` and the SDK's telemetry attributes (plus whatever `OTEL_RESOURCE_ATTRIBUTES` adds). `service.version`, `service.namespace` and `deployment.environment` are not set.

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
- **Shutdown:** on SIGTERM/SIGINT health goes `NOT_SERVING`, `GracefulStop` with a 10 s cap (then `Stop`), the sweep stops, the metrics server stops (5 s), telemetry is flushed (5 s), the pool is closed, exit 0.
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
- No spans for database calls; no `trace_id` in logs; no version/commit exposed.
- Not run: OTLP exporters against a real collector and Pyroscope, arm64 build, `govulncheck`, several replicas, any load or timing measurement beyond the 200-request oversell test. The legacy `COLLECTOR_SERVICE_ADDR` is still read (to be removed).

## License

To be decided by the owner. No `LICENSE` file exists in this repository yet.
