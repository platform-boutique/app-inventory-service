# inventory-service

A gRPC service that owns the stock of the Everly shop: how many pieces of each product can still be sold, and the reservations (holds) that checkout places while an order is being paid and shipped.

- `Reserve` takes stock all or nothing, `Commit` makes the sale final, `Release` and expiry give the stock back.
- It owns its own Postgres database and creates the schema and the seed stock (117 products) at start-up.
- Demo grade: no restock API, no admin, no caller authentication.

| | |
|---|---|
| Runtime | Go 1.26, static binary, image `platform-demo/inventory-service:local` (built locally only) |
| Ports | gRPC `8087`, HTTP `9090` (`/metrics`, scraped by Alloy) |
| State | its own Postgres (`stock`, `reservations`, `reservation_items`) |
| Called by | checkout-service, frontend |
| Status | not deployed anywhere yet; no pipeline, no Helm chart |

Pages: [architecture](architecture.md), [API](api.md), [configuration](configuration.md), [observability](observability.md), [operations](operations.md), [development](development.md), [decisions](adr/0001-one-postgres-per-service.md).
