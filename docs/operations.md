# Operations

## Health

- `grpc.health.v1.Health/Check` on the gRPC port (`8087`), for the empty service name and for `hipstershop.inventory.InventoryService`.
- `NOT_SERVING` from start until the migrations and the seed are done, and again whenever the Postgres ping fails (checked every 5 s). While not ready the handlers answer `UNAVAILABLE` (`database not ready`).
- Container probe: `/app/inventory-service healthcheck` calls the local health service (`127.0.0.1:$PORT`) and exits 0 only on `SERVING` (Docker `HEALTHCHECK`, interval 5 s, start period 10 s).

```bash
grpcurl -plaintext localhost:8087 grpc.health.v1.Health/Check
```

## Shutdown

SIGTERM or SIGINT: health goes `NOT_SERVING` so probes fail first; `GracefulStop` with a 10 s cap, then `Stop`; the sweep stops; the metrics server is shut down (5 s); telemetry is flushed (5 s); the pool is closed; exit 0.

Measured earlier by the author: `docker stop` exit code 0 (image smoke run). Not re-measured with a blackholed OTLP endpoint.

## Scaling and replicas

- Postgres is the only state. Several replicas are possible in principle: migrations and seed take advisory locks, every change of one order takes that order's advisory lock first, stock rows are locked in ascending id order.
- The sweep runs in every replica (harmless: the release is conditional on the state under row locks). With no replica running, holds are only released lazily.
- Running more than one replica was not tested (the tests use several `Service` values on one database, not several processes).
- Resource use in the local stack: `mem_limit: 128m` (workspace `local/compose.yaml`).

## Failure behaviour

| Failure | Behaviour |
|---|---|
| Postgres down at start | server listens, health `NOT_SERVING`, migration and seed retried every 2 s |
| Postgres goes away while running | health `NOT_SERVING` within about 5 s, handlers answer `UNAVAILABLE` (no SQL details), `SERVING` again after it returns |
| Restart | stock is NOT reset: the seed only inserts products without a row |
| Checkout dies after `Reserve` | the hold expires after `RESERVATION_TTL` (default 15 m) and the stock returns by the next call touching the products or by the sweep |
| Invalid `RESERVATION_TTL`, `SWEEP_INTERVAL` | process exits at start-up |
| Collector unreachable | telemetry export fails in the background; start-up does not depend on it |

## Clock behaviour

Expiry follows the wall clock of the process. `expires_at` is `now + RESERVATION_TTL` on the process clock and is compared with the process clock; there is no clock-skew protection between replicas and no monotonic or database-side time. A clock that steps forward makes holds expire early; one that steps back leaves holds alive for longer than the TTL. On the WSL2 machine this was developed on, the wall clock jumps about 58 s forward for roughly 60 ms every 5 s and steps back by tens or hundreds of milliseconds; with a 100 ms TTL about 1 hold in 100 (27 of 2546 in one measurement) got an `expires_at` up to 55 s away. With the default 15 minutes this is harmless; with tiny TTLs it is not. The stock books stay correct either way (the invariant does not depend on time); only the moment of release moves.

## Runbook

| Symptom | Check |
|---|---|
| Health stays `NOT_SERVING` | logs for `database not ready (attempt N)`: wrong `DATABASE_URL`, network, credentials |
| Product shows no stock although it was released | the hold is released lazily or by the sweep (`SWEEP_INTERVAL`, default 1 m); look for `sweep released N expired reservation(s)` in the log |
| `Commit` answers `FAILED_PRECONDITION` | the order id was released or its hold expired (`RESERVATION_TTL`) |
| Stock numbers differ from the catalog | the seed only inserts missing products; there is no restock API |
| Process exits at start | `DATABASE_URL is not set`, or an invalid `RESERVATION_TTL` / `SWEEP_INTERVAL` |

There is no dashboard or alert for this service yet.
