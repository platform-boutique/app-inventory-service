# 1. Stock lives in a Postgres owned by this service

Status: accepted (reconstructed from the code and the earlier README; no dated decision record existed).

## Context

Stock must not be oversold, must survive restarts and is changed concurrently by many orders. The shop's other data services (product catalog, reviews, orders, users) each keep their own database.

## Decision

inventory-service owns one Postgres database (`stock`, `reservations`, `reservation_items`). No other service reads it. The schema is created by embedded goose migrations run by the service at start-up, forward-only, under goose's advisory lock. The initial stock is an embedded seed inserted only for products without a row.

## Consequences

- Other services need the gRPC API; the catalog keeps its own copy of the stock numbers for display, and `tools/sync_stock.py` plus a drift test keep the seed in line with it.
- The service is not ready (`NOT_SERVING`) until the database is migrated, seeded and reachable.
- Overselling is prevented in the database: `CHECK (available >= 0)` plus a conditional decrement.
