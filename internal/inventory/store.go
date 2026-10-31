package inventory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Reservation states.
const (
	StatusReserved  = "RESERVED"
	StatusCommitted = "COMMITTED"
	StatusReleased  = "RELEASED"
)

// StockLevel is what can still be reserved for a product.
type StockLevel struct {
	ProductID string
	Available int32
}

// Shortage is a product that cannot cover a requested quantity.
type Shortage struct {
	ProductID string
	Requested int32
	Available int32
}

// ReserveResult is the outcome of a Reserve that did not fail: Reserved is false when at least one
// product is short (Shortages lists all of them, sorted by product id) and nothing was held.
type ReserveResult struct {
	Reserved  bool
	Shortages []Shortage
}

// Errors of the storage layer.
var (
	// ErrNotFound: no reservation with that order id (Commit).
	ErrNotFound = errors.New("reservation not found")
	// ErrItemsDiffer: the order id exists with other lines (Reserve).
	ErrItemsDiffer = errors.New("order id already reserved with different items")
	// ErrReleased: the reservation was released or has expired; the hold is gone (Reserve, Commit).
	ErrReleased = errors.New("reservation was released or expired")
	// ErrCommitted: the reservation is committed; sold stock is not returned (Release).
	ErrCommitted = errors.New("reservation is already committed")
	// errInconsistent: the data is not in a state the code can produce (a bug or a manual edit); maps to INTERNAL.
	errInconsistent = errors.New("inconsistent data")
	// ErrUnavailable marks storage errors caused by an unreachable database; the gRPC layer maps it to UNAVAILABLE.
	ErrUnavailable = errors.New("database unavailable")
)

// Store is what the gRPC handlers and the sweeper need from the storage layer. `now` always comes from the
// caller, so tests can move the clock.
type Store interface {
	// GetStock returns the levels of the known products among ids, in the order of ids. Reservations that
	// expired at now and hold one of these products are released first.
	GetStock(ctx context.Context, ids []string, now time.Time) ([]StockLevel, error)
	// Reserve holds the items (sorted by product id, unique) for the order, all or nothing, expiring at
	// now+ttl. ErrItemsDiffer, ErrReleased.
	Reserve(ctx context.Context, orderID string, items []Item, now time.Time, ttl time.Duration) (ReserveResult, error)
	// Commit turns RESERVED into COMMITTED. ErrNotFound, ErrReleased (also when it had expired at now: the
	// stock is returned in the same transaction).
	Commit(ctx context.Context, orderID string, now time.Time) error
	// Release returns the held quantities (RESERVED -> RELEASED). Unknown and already released order ids are
	// fine. ErrCommitted.
	Release(ctx context.Context, orderID string, now time.Time) error
	// Sweep releases up to limit reservations that expired at now and returns how many it released.
	Sweep(ctx context.Context, now time.Time, limit int) (int, error)
	// Ping checks that the database answers.
	Ping(ctx context.Context) error
}

// PGStore is the Postgres implementation of Store.
//
// Locking rules (they are what keeps it deadlock free and race free):
//  1. Every operation that changes one order takes that order's transaction-level advisory lock first. A
//     transaction holds at most one order lock, so order locks can never form a cycle.
//  2. Then the reservation row is locked (SELECT ... FOR UPDATE), then the stock rows, always in ascending
//     product id order.
//
// So Commit, Release, the expiry of a hold and a replayed Reserve of the same order are serialised, and two
// operations on different orders can only wait for each other on stock rows, in the same order.
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore wraps an existing pool (the caller owns and closes it).
func NewPGStore(pool *pgxpool.Pool) *PGStore { return &PGStore{pool: pool} }

func classify(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrNotFound) || errors.Is(err, ErrItemsDiffer) || errors.Is(err, ErrReleased) ||
		errors.Is(err, ErrCommitted) || errors.Is(err, ErrUnavailable) || errors.Is(err, errInconsistent) {
		return err
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// the server answered: a real SQL error, except for "server shutting down / too many connections" classes
		if strings.HasPrefix(pgErr.Code, "08") || strings.HasPrefix(pgErr.Code, "57") || pgErr.Code == "53300" {
			return fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		return err
	}
	// anything else (dial errors, closed connections, timeouts) means the database is not reachable
	return fmt.Errorf("%w: %v", ErrUnavailable, err)
}

// maxLazyExpiry bounds how many expired holds one GetStock/Reserve releases before it does its own work
// (the sweep takes the rest).
const maxLazyExpiry = 100

// reservation is a stored reservation header.
type reservation struct {
	status    string
	expiresAt time.Time
}

func lockOrder(ctx context.Context, tx pgx.Tx, orderID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('inventory-order:' || $1, 0))`, orderID)
	return err
}

// loadReservation locks and reads the reservation row; found is false when there is none.
func loadReservation(ctx context.Context, tx pgx.Tx, orderID string) (r reservation, found bool, err error) {
	err = tx.QueryRow(ctx, `SELECT status, expires_at FROM reservations WHERE order_id = $1 FOR UPDATE`, orderID).
		Scan(&r.status, &r.expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return reservation{}, false, nil
	}
	return r, err == nil, err
}

// loadItems reads the lines of a reservation sorted by product id.
func loadItems(ctx context.Context, tx pgx.Tx, orderID string) ([]Item, error) {
	rows, err := tx.Query(ctx, `SELECT product_id, quantity FROM reservation_items WHERE order_id = $1 ORDER BY product_id`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ProductID, &it.Quantity); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// releaseLocked gives a RESERVED reservation's quantities back and marks it RELEASED, in the caller's
// transaction (which holds the order lock and the reservation row lock). Stock rows are locked by the
// UPDATEs in ascending product id order.
func releaseLocked(ctx context.Context, tx pgx.Tx, orderID string, now time.Time) error {
	items, err := loadItems(ctx, tx, orderID)
	if err != nil {
		return err
	}
	for _, it := range items {
		tag, err := tx.Exec(ctx, `UPDATE stock SET available = available + $2 WHERE product_id = $1`, it.ProductID, it.Quantity)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: stock row of a reserved product is missing (order %s)", errInconsistent, orderID)
		}
	}
	tag, err := tx.Exec(ctx,
		`UPDATE reservations SET status = $2, updated_at = $3 WHERE order_id = $1 AND status = $4`,
		orderID, StatusReleased, now, StatusReserved)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: reservation changed while it was locked (order %s)", errInconsistent, orderID)
	}
	return nil
}

// releaseExpiredOne releases one reservation if, under its locks, it is still RESERVED and expired at now.
func (s *PGStore) releaseExpiredOne(ctx context.Context, orderID string, now time.Time) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, classify(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockOrder(ctx, tx, orderID); err != nil {
		return false, classify(err)
	}
	r, found, err := loadReservation(ctx, tx, orderID)
	if err != nil {
		return false, classify(err)
	}
	if !found || r.status != StatusReserved || r.expiresAt.After(now) {
		return false, nil // committed, released or renewed meanwhile: nothing to do
	}
	if err := releaseLocked(ctx, tx, orderID, now); err != nil {
		return false, classify(err)
	}
	return true, classify(tx.Commit(ctx))
}

// expireTouching releases the expired holds that include one of the products.
func (s *PGStore) expireTouching(ctx context.Context, ids []string, now time.Time) error {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT r.order_id
		   FROM reservations r JOIN reservation_items i ON i.order_id = r.order_id
		  WHERE r.status = 'RESERVED' AND r.expires_at <= $1 AND i.product_id = ANY($2)
		  LIMIT $3`, now, ids, maxLazyExpiry)
	if err != nil {
		return classify(err)
	}
	orders, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return classify(err)
	}
	for _, o := range orders {
		if _, err := s.releaseExpiredOne(ctx, o, now); err != nil {
			return err
		}
	}
	return nil
}

// GetStock implements Store.
func (s *PGStore) GetStock(ctx context.Context, ids []string, now time.Time) ([]StockLevel, error) {
	if err := s.expireTouching(ctx, ids, now); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT product_id, available FROM stock WHERE product_id = ANY($1)`, ids)
	if err != nil {
		return nil, classify(err)
	}
	defer rows.Close()
	have := make(map[string]int32, len(ids))
	for rows.Next() {
		var id string
		var n int32
		if err := rows.Scan(&id, &n); err != nil {
			return nil, classify(err)
		}
		have[id] = n
	}
	if err := rows.Err(); err != nil {
		return nil, classify(err)
	}
	out := make([]StockLevel, 0, len(have))
	for _, id := range ids {
		if n, ok := have[id]; ok {
			out = append(out, StockLevel{ProductID: id, Available: n})
		}
	}
	return out, nil
}

// Reserve implements Store.
func (s *PGStore) Reserve(ctx context.Context, orderID string, items []Item, now time.Time, ttl time.Duration) (ReserveResult, error) {
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ProductID
	}
	if err := s.expireTouching(ctx, ids, now); err != nil {
		return ReserveResult{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ReserveResult{}, classify(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockOrder(ctx, tx, orderID); err != nil {
		return ReserveResult{}, classify(err)
	}

	// An existing reservation decides first: replay, other items, released or expired.
	r, found, err := loadReservation(ctx, tx, orderID)
	if err != nil {
		return ReserveResult{}, classify(err)
	}
	if found {
		switch {
		case r.status == StatusReleased:
			return ReserveResult{}, ErrReleased
		case r.status == StatusReserved && !r.expiresAt.After(now):
			// expired: the hold is gone. Give the stock back and KEEP that (commit), then refuse.
			if err := releaseLocked(ctx, tx, orderID, now); err != nil {
				return ReserveResult{}, classify(err)
			}
			if err := tx.Commit(ctx); err != nil {
				return ReserveResult{}, classify(err)
			}
			return ReserveResult{}, ErrReleased
		}
		held, err := loadItems(ctx, tx, orderID)
		if err != nil {
			return ReserveResult{}, classify(err)
		}
		if !SameItems(held, items) {
			return ReserveResult{}, ErrItemsDiffer
		}
		return ReserveResult{Reserved: true}, nil // replay: nothing changes
	}

	// New order: lock the needed stock rows in ascending product id order, then look at every shortage.
	rows, err := tx.Query(ctx, `SELECT product_id, available FROM stock WHERE product_id = ANY($1) ORDER BY product_id FOR UPDATE`, ids)
	if err != nil {
		return ReserveResult{}, classify(err)
	}
	have := make(map[string]int32, len(ids))
	for rows.Next() {
		var id string
		var n int32
		if err := rows.Scan(&id, &n); err != nil {
			rows.Close()
			return ReserveResult{}, classify(err)
		}
		have[id] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ReserveResult{}, classify(err)
	}
	var short []Shortage
	for _, it := range items { // items are sorted by product id
		if avail := have[it.ProductID]; avail < it.Quantity { // unknown product: avail is 0
			short = append(short, Shortage{ProductID: it.ProductID, Requested: it.Quantity, Available: avail})
		}
	}
	if len(short) > 0 {
		return ReserveResult{Reserved: false, Shortages: short}, nil // nothing was changed; the deferred rollback ends the tx
	}

	for _, it := range items {
		tag, err := tx.Exec(ctx,
			`UPDATE stock SET available = available - $2 WHERE product_id = $1 AND available >= $2`, it.ProductID, it.Quantity)
		if err != nil {
			return ReserveResult{}, classify(err)
		}
		if tag.RowsAffected() != 1 { // cannot happen under the row locks; refuse rather than oversell
			return ReserveResult{}, fmt.Errorf("%w: conditional stock update matched %d rows (order %s)", errInconsistent, tag.RowsAffected(), orderID)
		}
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO reservations (order_id, status, created_at, updated_at, expires_at) VALUES ($1, $2, $3, $3, $4)`,
		orderID, StatusReserved, now, now.Add(ttl)); err != nil {
		return ReserveResult{}, classify(err)
	}
	pids := make([]string, len(items))
	qtys := make([]int32, len(items))
	for i, it := range items {
		pids[i], qtys[i] = it.ProductID, it.Quantity
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO reservation_items (order_id, product_id, quantity) SELECT $1, * FROM unnest($2::text[], $3::int4[])`,
		orderID, pids, qtys); err != nil {
		return ReserveResult{}, classify(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return ReserveResult{}, classify(err)
	}
	return ReserveResult{Reserved: true}, nil
}

// Commit implements Store.
func (s *PGStore) Commit(ctx context.Context, orderID string, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return classify(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockOrder(ctx, tx, orderID); err != nil {
		return classify(err)
	}
	r, found, err := loadReservation(ctx, tx, orderID)
	if err != nil {
		return classify(err)
	}
	switch {
	case !found:
		return ErrNotFound
	case r.status == StatusCommitted:
		return nil
	case r.status == StatusReleased:
		return ErrReleased
	case !r.expiresAt.After(now):
		if err := releaseLocked(ctx, tx, orderID, now); err != nil {
			return classify(err)
		}
		if err := tx.Commit(ctx); err != nil {
			return classify(err)
		}
		return ErrReleased
	}
	// Conditional on the state: the sweep takes the same locks, so exactly one of Commit and the expiry wins.
	tag, err := tx.Exec(ctx,
		`UPDATE reservations SET status = $2, updated_at = $3 WHERE order_id = $1 AND status = $4`,
		orderID, StatusCommitted, now, StatusReserved)
	if err != nil {
		return classify(err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: reservation changed while it was locked (order %s)", errInconsistent, orderID)
	}
	return classify(tx.Commit(ctx))
}

// Release implements Store.
func (s *PGStore) Release(ctx context.Context, orderID string, now time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return classify(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockOrder(ctx, tx, orderID); err != nil {
		return classify(err)
	}
	r, found, err := loadReservation(ctx, tx, orderID)
	if err != nil {
		return classify(err)
	}
	switch {
	case !found, r.status == StatusReleased:
		return nil
	case r.status == StatusCommitted:
		return ErrCommitted
	}
	if err := releaseLocked(ctx, tx, orderID, now); err != nil {
		return classify(err)
	}
	return classify(tx.Commit(ctx))
}

// Sweep implements Store.
func (s *PGStore) Sweep(ctx context.Context, now time.Time, limit int) (int, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT order_id FROM reservations WHERE status = 'RESERVED' AND expires_at <= $1 ORDER BY expires_at LIMIT $2`, now, limit)
	if err != nil {
		return 0, classify(err)
	}
	orders, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, classify(err)
	}
	released := 0
	for _, o := range orders {
		ok, err := s.releaseExpiredOne(ctx, o, now)
		if err != nil {
			return released, err
		}
		if ok {
			released++
		}
	}
	return released, nil
}

// Ping implements Store.
func (s *PGStore) Ping(ctx context.Context) error {
	var one int
	return classify(s.pool.QueryRow(ctx, `SELECT 1`).Scan(&one))
}
