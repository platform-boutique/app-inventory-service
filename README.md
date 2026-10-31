# inventory-service

A gRPC service that owns the shop's stock: how many pieces of each product can still be sold, and the reservations (holds) that checkout places while an order is being paid and shipped. It owns its own Postgres database and creates the schema and the seed stock with embedded migrations at start-up. Demo grade (see [Limits](#limits)): no restock API, no admin, no caller authentication.

## At a glance

| | |
|---|---|
| **Language / runtime** | Go 1.26 (static binary, `CGO_ENABLED=0`) |
| **Image** | built locally only: `inventory-service:local` (about 12 MB) |
| **Protocol / port** | gRPC (HTTP/2, plaintext) on `8087` |
| **Metrics** | Prometheus `/metrics` on `9090`; pushed over OTLP/gRPC only when an endpoint is set |
| **Health** | Standard `grpc.health.v1.Health/Check` on `8087`: `NOT_SERVING` until the migrations and the seed are done and whenever the database does not answer a ping (checked every 5 s) |
| **State** | Postgres (`stock`, `reservations`, `reservation_items`), via `DATABASE_URL` |
| **Depends on** | Its own Postgres (optional: OTLP collector, Pyroscope) |
| **Called by** | checkout-service (Reserve, Commit, Release) and the frontend (GetStock), once they are wired up; nothing calls it yet in the local stack |
| **gRPC reflection** | Enabled: `grpcurl` works without `-proto` |

## API

Package `hipstershop.inventory`, service `InventoryService` ([`proto/inventory.proto`](proto/inventory.proto), copied verbatim from the slice-4 contract; checkout and the frontend copy it from here).

| Method | Request | Behaviour | Error codes |
|---|---|---|---|
| `GetStock` | `product_ids` (1-100) | Levels (`available` = what can still be reserved right now) of the known products, in request order. Unknown ids are missing from the answer; duplicate ids are answered once. Expired holds on these products are released first. | `INVALID_ARGUMENT` (no ids, more than 100, empty/over-long id), `UNAVAILABLE`, `INTERNAL` |
| `Reserve` | `order_id`, `items` (1-100 lines of `product_id`, `quantity` 1-1000) | All or nothing, one transaction. Enough stock for every line: takes the quantities and answers `reserved = true`. Any line short (an unknown product counts as `available = 0`): `reserved = false`, EVERY short product in `shortages` (sorted by product id), nothing held and no row stored. Replay with the same order id and the same lines (any order) while RESERVED or COMMITTED: `reserved = true`, nothing changes. | `INVALID_ARGUMENT` (bad order id, no lines, duplicate product, quantity outside 1-1000, over 100 lines, bad product id), `ALREADY_EXISTS` (same order id, different lines), `FAILED_PRECONDITION` (the order id was RELEASED or its hold expired), `UNAVAILABLE`, `INTERNAL` |
| `Commit` | `order_id` | RESERVED -> COMMITTED (the stock stays taken). Idempotent. | `NOT_FOUND` (unknown order id), `FAILED_PRECONDITION` (RELEASED, or expired: the hold is gone and its stock is returned in the same call), `INVALID_ARGUMENT`, `UNAVAILABLE`, `INTERNAL` |
| `Release` | `order_id` | RESERVED -> RELEASED: the held quantities go back to `available` in the same transaction. Idempotent: an unknown or already released order id is `OK` (no row is created for an unknown one). | `FAILED_PRECONDITION` (COMMITTED: sold stock is not returned), `INVALID_ARGUMENT`, `UNAVAILABLE`, `INTERNAL` |

Field rules: `order_id` is 1-64 characters of `[A-Za-z0-9._-]`, starting with a letter or digit (the order-service rule; surrounding white space is trimmed). `product_id` is 1-128 characters of valid UTF-8 without NUL (trimmed). The order of the checks in `Reserve` for an existing order id is: RELEASED or expired -> `FAILED_PRECONDITION`, then different lines -> `ALREADY_EXISTS`, else replay.

All handlers answer `UNAVAILABLE` (`database not ready`) until the migrations have run and whenever the health check fails; a database outage gives `UNAVAILABLE` (`database unavailable`) without SQL details (those are logged); a handler panic becomes `INTERNAL` through a recovery interceptor.

```bash
grpcurl -plaintext -d '{"product_ids":["blue-frock-162"]}' localhost:8087 hipstershop.inventory.InventoryService/GetStock
grpcurl -plaintext -d '{"order_id":"o-1","items":[{"product_id":"blue-frock-162","quantity":2}]}' localhost:8087 hipstershop.inventory.InventoryService/Reserve
grpcurl -plaintext -d '{"order_id":"o-1"}' localhost:8087 hipstershop.inventory.InventoryService/Commit
grpcurl -plaintext -d '{"order_id":"o-1"}' localhost:8087 hipstershop.inventory.InventoryService/Release
```

## State machine

```
            Reserve (enough stock)                 Commit
 (none) ----------------------------> RESERVED ------------------> COMMITTED   (final, never expires, never returned)
                                        |   |
                      Release           |   | expires_at <= now (lazy or sweep)
                                        v   v
                                       RELEASED                                  (final; the order id cannot be reserved again)
```

- `Reserve` takes `available` down by the quantities and stores the reservation (`expires_at = now + RESERVATION_TTL`). A refused `Reserve` (shortage) stores nothing, so the same order id may be tried again later with any lines.
- `Release` and expiry add the quantities back, exactly once (the status change and the stock update are one transaction, and the status change is conditional on `RESERVED`).
- Expiry: a RESERVED hold with `expires_at <= now` counts as gone. It is turned into RELEASED (stock returned) **lazily** by the first `GetStock`/`Reserve` that involves one of its products (at most 100 holds per call, each in its own transaction, before the call does its own work), by a `Commit`/`Reserve`/`Release` of that very order id (same transaction), or by the **background sweep** (every `SWEEP_INTERVAL`, default one minute, per process, 200 holds per batch until none is left). There is no cron.
- Invariant (checked by the integration tests after every race): for each product, `available + quantities of RESERVED and COMMITTED reservations = initial stock`, and `available` is never negative.

## How a reservation is made safe

- `available` has `CHECK (available >= 0)`; the decrement is `UPDATE stock SET available = available - q WHERE product_id = $1 AND available >= q` and the row count must be 1.
- `Reserve` locks the needed stock rows with one `SELECT ... WHERE product_id = ANY(...) ORDER BY product_id FOR UPDATE` (ascending id), so two orders with the same products in opposite line order wait for each other in the same order and cannot deadlock. Then it computes all shortages; only if there are none does it decrement and insert the reservation.
- Every change of ONE order first takes that order's transaction-level advisory lock (`pg_advisory_xact_lock`), then the reservation row (`FOR UPDATE`), then stock rows in ascending order. A transaction holds at most one order lock, so order locks cannot form a cycle. This serialises `Commit`, `Release`, the expiry of that hold and a replayed `Reserve` of the same order, so exactly one of `Commit` and the expiry wins and a committed reservation never has its stock returned.
- Time comes from the process clock (`now` is passed to the store). Replicas with clocks that differ by more than a few seconds would disagree about the exact moment of expiry, never about stock.

## Configuration

| Variable | Default | Description |
|---|---|---|
| `DATABASE_URL` | **required** | Postgres URL, e.g. `postgres://inventory:inventory@inventory-db:5432/inventory?sslmode=disable`. The process exits if it is unset. A 5 s connect timeout is added unless the URL sets one |
| `PORT` | `8087` | gRPC listen port |
| `METRICS_PORT` | `9090` | Prometheus (and, with profiling, pprof) HTTP port |
| `RESERVATION_TTL` | `15m` | How long a RESERVED hold lives (Go duration). Invalid or non-positive: the process exits |
| `SWEEP_INTERVAL` | `1m` | How often the background sweep looks for expired holds (Go duration; not in the contract, added so tests and demos need not wait a minute). Invalid or non-positive: the process exits |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | unset (telemetry off) | OTLP/**gRPC** endpoint for traces, metrics and logs (scheme required). Legacy fallback: `COLLECTOR_SERVICE_ADDR` (`host:port`). Telemetry is on only if one of them is set and non-empty |
| `OTEL_SDK_DISABLED` | - | `true` forces telemetry off even with an endpoint (the legacy `ENABLE_TRACING=0` does the same) |
| `OTEL_METRIC_EXPORT_INTERVAL` | `15000` | Milliseconds between metric pushes |
| `OTEL_SERVICE_NAME` | `inventory-service` | `service.name` |
| `PYROSCOPE_ADDR` | unset (profiling off) | Profiling (Pyroscope push **and** the pprof endpoints on `METRICS_PORT`) is on only if this is set and `ENABLE_PROFILING` is not `0` |
| `ENABLE_PROFILING` | - | `0` forces profiling off |

Logs are JSON on stdout (logrus, fields `timestamp`, `severity`, `message`). A handler logs the order id and counts only (`reserve order=o-1 lines=2 reserved=false shortages=1`, `commit order=o-1`, `sweep released 3 expired reservation(s)`); product ids and quantities are never logged (a unit and an integration test check that, each with a control that proves the log lines were really there).

Start-up: the gRPC server listens at once (health `NOT_SERVING`); a background loop runs the goose migrations (under goose's Postgres session advisory lock, so replicas starting together apply each migration once) and the seed, retrying every 2 s until the database is reachable, then starts the sweep. SIGTERM/SIGINT: health goes `NOT_SERVING`, `GracefulStop` (10 s cap), the sweep stops, the pool is closed, exit 0.

## Seed stock

`data/stock.json` (embedded in the binary) holds `product_id -> available` copied from the `stock` field of the catalog seed, `apps/product-catalog-service/data/seed/products.json` (117 products, 0-100 pieces). At start the service inserts every product that has no stock row yet: `INSERT ... ON CONFLICT DO NOTHING`. It never changes an existing row, so a restart (or a second replica) never resets live stock, and changing a number in the catalog later does **not** change the stock of a product that is already in the database.

Keeping the copy honest:

```bash
python3 tools/sync_stock.py            # rewrites data/stock.json from ../product-catalog-service/data/seed/products.json
python3 tools/sync_stock.py --check    # exit 0 when it matches, 1 with the differences, 2 when the catalog file is missing
CATALOG_SEED=/path/to/products.json go test ./internal/inventory -run TestSeedMatchesCatalog -v
```

`TestSeedMatchesCatalog` fails when the copy has drifted (a changed number, a product added to or removed from the catalog); it is skipped, with a message saying that drift was NOT checked, when the catalog file cannot be found (default: the sibling checkout, or `CATALOG_SEED`).

## Storage

`stock(product_id pk, available int check >= 0)`, `reservations(order_id pk, status RESERVED|COMMITTED|RELEASED, created_at, updated_at, expires_at)` (partial index on `expires_at` for RESERVED rows) and `reservation_items(order_id, product_id, quantity 1-1000, pk (order_id, product_id))` with foreign keys to both. Check constraints repeat the id lengths. One goose migration, embedded in the binary.

## Container image

Multi-stage [`Dockerfile`](Dockerfile) (same as user-service): `golang:1.26-alpine` builder (`--platform=$BUILDPLATFORM`, `GOARCH=$TARGETARCH`), `alpine:3.24` runtime with the single binary at `/app/inventory-service`.

- **User:** `10001:10001` (numeric, non-root).
- **HEALTHCHECK:** `/app/inventory-service healthcheck` calls the gRPC health service on `127.0.0.1:$PORT` and exits 0 only on `SERVING`.
- **EXPOSE:** `8087`, `9090`. Built locally on amd64 (`docker build -t inventory-service:local .`); arm64 was not built.

## Limits

- **The sweep is per process.** With N replicas every replica sweeps (harmless: the release is conditional on the state under row locks), and with no replica running, holds are only released lazily. The lazy path means an expired hold never blocks a sale of the same product for longer than the next call that mentions it.
- **No restock API, no admin, no way to change stock at runtime.** Stock only goes down by sales and comes back by release/expiry. New products appear after a restart with a newer seed; changed numbers for existing products do not.
- No caller authentication; plain gRPC without TLS (in the local stack it is on the internal network only).
- A `Commit` that arrives after `expires_at` is refused even if the sweep has not run yet (the stock is returned in that call). Checkout treats a failed `Commit` after charge and shipment as a known gap (see the contract).
- A lazy release by `GetStock`/`Reserve` handles at most 100 expired holds per call; the sweep (200 per batch, repeated) takes the rest.
- **Expiry follows the wall clock of the process.** `expires_at` is `now + RESERVATION_TTL` on the process clock and is compared with the process clock; there is no clock-skew protection between replicas and no monotonic or database-side time. A clock that steps forward makes holds expire early, one that steps forward and back (or back) leaves holds alive for longer than the TTL: on the WSL2 machine this was developed on, the wall clock jumps about 58 s forward for roughly 60 ms every 5 s and steps back by tens or hundreds of milliseconds, and with a 100 ms TTL about 1 hold in 100 (27 of 2546 in one measurement) got an `expires_at` up to 55 s away. With the default 15 minutes this is harmless; with tiny TTLs it is not. The stock books stay correct either way (the invariant does not depend on time); only the moment of release moves.
- Reserved quantities are not tied to a price or a user; the service only knows order ids, products and counts.

## Running locally

Requirements: Go 1.26 (`GOTOOLCHAIN=auto` fetches it), Docker, [`grpcurl`](https://github.com/fullstorydev/grpcurl), Python 3 for the sync tool.

What was actually run (all on the final tree):

```bash
# unit tests (validation, item comparison, service on an in-memory store, seed + catalog drift, config, logs)
go vet ./... && gofmt -l . && go test -race ./...

# integration tests against a throwaway Postgres (each test creates and drops its own database)
docker run -d --rm --name inv-test-$RANDOM --memory 256m -e POSTGRES_USER=inv -e POSTGRES_PASSWORD=inv \
  -p 127.0.0.1:55437:5432 postgres:17-alpine
TEST_DATABASE_URL='postgres://inv:inv@127.0.0.1:55437/postgres?sslmode=disable' \
  go test -race -tags integration ./tests/integration/

# container smoke run: image + a Postgres with its own database and user on one docker network
docker build -t inventory-service:local .
docker network create inv-smoke-net
docker run -d --name inv-smoke-db --memory 256m --network inv-smoke-net --network-alias inventory-db \
  -e POSTGRES_DB=inventory -e POSTGRES_USER=inventory -e POSTGRES_PASSWORD=inventory postgres:17-alpine
docker run -d --name inv-smoke-svc --memory 128m --network inv-smoke-net -p 127.0.0.1:58087:8087 \
  -e RESERVATION_TTL=20s -e SWEEP_INTERVAL=5s \
  -e DATABASE_URL='postgres://inventory:inventory@inventory-db:5432/inventory?sslmode=disable' inventory-service:local
```

Results: `go vet` (also with `-tags integration`), `gofmt -l` (empty), `go test -race ./...` and the integration tests with `-race` pass; `golangci-lint run` (default linters, also with `--build-tags integration`) and `gosec -exclude-generated ./...` report no issues.

The integration tests (19) run on an injected clock (time moves with `Advance`, never with a real sleep, see the comment in the test file) and cover: migrations idempotent, six concurrent starts apply exactly one migration and seed once; seeding twice never resets live stock (reserved and committed quantities survive `Seed` and `Setup`) and a new product is added without touching others; the lifecycle with the exact availability after each step (reserve, reserve everything, commit, release, reserve again); shortages for several products at once (including an unknown one) with nothing held and no rows stored; unknown product = shortage with `available = 0` and missing from `GetStock`; idempotent replays of Reserve (lines in another order), Commit and Release; the same order id with different lines = `ALREADY_EXISTS` (four variants); Release after Release (stock returned once); Commit after Release, Reserve of a released id, Release after Commit = `FAILED_PRECONDITION`; Commit of an unknown order = `NOT_FOUND`, Release of an unknown order = `OK` without a row; expiry with a 300 ms TTL (clock advanced by 400 ms; lazy release by Commit, by GetStock, by Reserve over an expired hold, replay of an expired order id refused, a committed hold never expires, the sweep) and the background sweeper returning stock with no call touching it (and not before the hold is expired); the oversell test (200 parallel Reserve of 1 unit against stock 50: exactly 50 `reserved`, 150 refused, no errors, `available = 0`, 50 reservations, books balanced); multi-line reservations in opposite line orders (200 in parallel through the service, 200 more straight into the store with unsorted lines) finish without a deadlock; 150 mixed concurrent reserve/commit/release/replay operations on three products keep the books balanced; 30 parallel replays of one order take stock once, and 30 parallel Reserves of one order id with two different line sets leave exactly one winner; Commit racing the expiry sweep (200 reservations, two services whose clocks are before and after the expiry, 8 sweepers; in 3 runs the commit won 181-184 and the sweep 16-19 times, every outcome consistent with the Commit result and no committed reservation with returned stock). A mutation check (sweep ignoring the state and the order lock) made this test fail, so it does detect the bug it is for. Also: check constraints, database unreachable and closed pool give `UNAVAILABLE` without SQL text, host, password, table names, no secrets/product ids in logs, and the whole flow over a real gRPC connection.

The container smoke run checked with `grpcurl` (reflection, no `-proto`): service list and `describe`; `GetStock` with an unknown id; Reserve (ok, shortage with an unknown product, other lines = `ALREADY_EXISTS`, invalid order id = `INVALID_ARGUMENT`); Commit twice, Release after Commit = `FAILED_PRECONDITION`, Commit of an unknown order = `NOT_FOUND`, Release of an unknown order = `OK`; a hold with `RESERVATION_TTL=20s` was released by the sweeper (`SWEEP_INTERVAL=5s`, log line `sweep released 1 expired reservation(s)`) and a later Commit/Reserve of that id = `FAILED_PRECONDITION`; health `SERVING` and Docker health `healthy`; `/metrics` (read inside the container) shows `grpc_server_handled_total` for the four methods; after `docker restart` the stock was NOT reset (`0 migration(s) applied, 0 stock row(s) seeded`); `NOT_SERVING` and `UNAVAILABLE` after disconnecting the database from the network, `SERVING` and answers again after reconnecting; uid `10001`; exit code 0 on `docker stop`.

Not run: the OTLP exporters against a real collector and Pyroscope (the code is the user-service/order-service code; only the "off by default / forced off" switches are unit-tested), an arm64 build, `govulncheck`, the service inside the compose stack and any call from checkout or the frontend, several replicas against one database (the lock design covers it, the tests use several `Service` values on one database, not several processes), any load or timing measurement beyond the 200-request oversell test.

Regenerate the Go code after editing the proto: `protoc -I proto --go_out=proto --go_opt=paths=source_relative --go-grpc_out=proto --go-grpc_opt=paths=source_relative proto/inventory.proto` (protoc 3.21.12, protoc-gen-go 1.36.11, protoc-gen-go-grpc 1.6.1). Proto changes are additive only.

## Project structure

```
├── cmd/server/
│   ├── main.go            # entrypoint: telemetry, profiling, gRPC server, DB watcher + sweeper, graceful shutdown
│   ├── config.go          # RESERVATION_TTL, SWEEP_INTERVAL
│   ├── otlp.go            # OTLP traces/metrics/logs (same as order-service)
│   └── healthcheck.go     # `inventory-service healthcheck` for the container HEALTHCHECK
├── internal/inventory/
│   ├── service.go         # gRPC handlers, error mapping, sweeper
│   ├── store.go           # Postgres store: locking, reserve/commit/release/expiry transactions
│   ├── validate.go        # order id / product id / quantity rules, order-insensitive item comparison
│   ├── migrate.go         # goose migrations, seed parsing and idempotent seeding
│   └── migrations/        # 00001_create_inventory.sql
├── data/                  # embedded seed stock (stock.json, written by tools/sync_stock.py)
├── tools/sync_stock.py    # copies / checks the stock numbers of the catalog seed
├── proto/                 # inventory.proto + generated Go
├── tests/integration/     # -tags integration, needs TEST_DATABASE_URL
├── Dockerfile
└── .pre-commit-config.yaml
```
