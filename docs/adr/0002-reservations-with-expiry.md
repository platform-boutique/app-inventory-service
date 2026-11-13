# 2. Stock is held by reservations that expire

Status: accepted (reconstructed from the code and the earlier README).

## Context

Checkout reserves stock before it charges the card and ships. If checkout dies in between, the stock must not stay taken forever.

## Decision

`Reserve` takes the quantities all or nothing and stores a reservation with `expires_at = now + RESERVATION_TTL`. `Commit` makes it final, `Release` returns the stock. An uncommitted hold past its expiry counts as gone: it is released lazily by the next call that involves its products, by a `Commit`/`Reserve`/`Release` of that order id, and by a background sweep (every `SWEEP_INTERVAL`). Every change of one order takes that order's advisory lock first, then the reservation row, then stock rows in ascending id order.

## Consequences

- Exactly one of `Commit` and the expiry wins; a committed reservation never has its stock returned.
- Opposite line orders cannot deadlock.
- Expiry follows the process wall clock; with the default TTL (15 m) clock jumps are harmless, with tiny TTLs they are not.
- A `Commit` after expiry is refused; checkout treats a failed `Commit` after charge and shipment as a known gap.
