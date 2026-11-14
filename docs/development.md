# Develop and test

Requirements: Go 1.26 (`GOTOOLCHAIN=auto` fetches the toolchain from `go.mod`), Docker for integration tests, `grpcurl`, `golangci-lint` v2.

Commands run on 2026-11-13 for the standards change (host: Go 1.22 with `GOTOOLCHAIN=auto` selecting 1.26.10):

| Command | Result |
|---|---|
| `go vet ./... && go test -race -count=1 ./...` | passed (2 packages with tests) |
| `golangci-lint run ./...` and `golangci-lint run --build-tags integration ./...` (golangci-lint v2.11.4, `.golangci.yaml`) | 0 issues. The shared pipeline pins v2.14.0: not run |
| `gofmt -l .` | no files listed |
| `mkdocs build --strict` (mkdocs 1.6.1, mkdocs-techdocs-core 1.7.1, Python 3.12 venv made with uv) | passed |

## What the earlier author ran (recorded, not re-run for this page)

Commands and results below come from the README before the standards change; they were not repeated on 2026-11-13.

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

Observability step 3 (2026-11-14): `go vet` (also with `-tags integration`), `go test ./...` and the integration tests against a throwaway Postgres 17 container pass; `internal/obs` (the same package as in checkout-service) has the env-matrix, log-shape, resource-attribute and blackholed-collector tests; `golangci-lint run` (the repo's `.golangci.yaml`) reports 0 issues; not repeated: `-race`, `gosec`.

Not run: the OTLP exporters against a real collector and Pyroscope (only the env switches, the bounded start and the bounded shutdown against a listener that never answers are tested), an arm64 build, `govulncheck`, the service inside the compose stack and any call from checkout or the frontend, several replicas against one database (the lock design covers it, the tests use several `Service` values on one database, not several processes), any load or timing measurement beyond the 200-request oversell test.

Regenerate the Go code after editing the proto: `protoc -I proto --go_out=proto --go_opt=paths=source_relative --go-grpc_out=proto --go-grpc_opt=paths=source_relative proto/inventory.proto` (protoc 3.21.12, protoc-gen-go 1.36.11, protoc-gen-go-grpc 1.6.1). Proto changes are additive only.

## Build the docs

```bash
uv venv docs-venv && uv pip install --python docs-venv/bin/python 'mkdocs==1.6.1' 'mkdocs-techdocs-core==1.7.1'
docs-venv/bin/mkdocs build --strict
```

Mermaid diagrams are plain fenced code blocks for `mkdocs`; rendering them in Backstage needs the TechDocs Mermaid add-on (not verified here).
