# 3. The seed only inserts products that have no stock row

Status: accepted (reconstructed from the code and the earlier README).

## Context

The catalog seed holds an initial stock per product. Live stock changes with every sale and must survive restarts and second replicas.

## Decision

At start `data/stock.json` (embedded) is inserted with `INSERT ... ON CONFLICT DO NOTHING`. Existing rows are never touched. `tools/sync_stock.py` keeps the embedded copy in line with the catalog seed, and `TestSeedMatchesCatalog` fails on drift.

## Consequences

- A restart never resets live stock.
- New products appear after a restart with a newer seed; changed numbers for existing products do not. There is no restock API.
