# API

Package `hipstershop.inventory`, service `InventoryService` ([`proto/inventory.proto`](https://github.com/platform-boutique/app-inventory-service/blob/main/proto/inventory.proto), copied verbatim from the slice-4 contract; checkout and the frontend copy it from here).

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
