package inventory

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"

	"inventory-service/data"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migrate applies all embedded migrations. goose holds a Postgres session advisory lock while it runs,
// so replicas that start at the same time apply each migration exactly once.
func Migrate(ctx context.Context, pool *pgxpool.Pool) ([]*goose.MigrationResult, error) {
	sub, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	locker, err := lock.NewPostgresSessionLocker(
		lock.WithLockTimeout(1, 120), // retry every second for up to two minutes
	)
	if err != nil {
		return nil, err
	}
	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, sub, goose.WithSessionLocker(locker))
	if err != nil {
		return nil, err
	}
	res, err := provider.Up(ctx)
	if err != nil {
		return res, classify(err)
	}
	return res, nil
}

// SeedStock is one entry of data/stock.json (written by tools/sync_stock.py).
type SeedStock struct {
	ProductID string `json:"product_id"`
	Available int32  `json:"available"`
}

// ParseSeed decodes and validates seed data; any invalid entry fails the whole file. The result is sorted
// by product id (the order in which rows are inserted, so concurrent seeders cannot deadlock).
func ParseSeed(raw []byte) ([]SeedStock, error) {
	var items []SeedStock
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("seed: %w", err)
	}
	seen := make(map[string]struct{}, len(items))
	for i := range items {
		id, err := ValidateProductID("product_id", items[i].ProductID)
		if err != nil {
			return nil, fmt.Errorf("seed entry %d: %w", i, err)
		}
		if items[i].Available < 0 {
			return nil, fmt.Errorf("seed entry %d (%s): available %d is negative", i, id, items[i].Available)
		}
		if _, dup := seen[id]; dup {
			return nil, fmt.Errorf("seed entry %d: product %q appears twice", i, id)
		}
		seen[id] = struct{}{}
		items[i].ProductID = id
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ProductID < items[j].ProductID })
	return items, nil
}

// Seed inserts the stock of products that have no stock row yet and returns how many rows it inserted.
// INSERT ... ON CONFLICT DO NOTHING: it never touches a row that exists, so live stock (reservations,
// sales) is never reset, and concurrent starts are harmless.
func Seed(ctx context.Context, pool *pgxpool.Pool, raw []byte) (int64, error) {
	items, err := ParseSeed(raw)
	if err != nil {
		return 0, err
	}
	ids := make([]string, len(items))
	qty := make([]int32, len(items))
	for i, it := range items {
		ids[i], qty[i] = it.ProductID, it.Available
	}
	tag, err := pool.Exec(ctx,
		`INSERT INTO stock (product_id, available)
		   SELECT * FROM unnest($1::text[], $2::int4[]) AS t(product_id, available)
		   ORDER BY product_id
		 ON CONFLICT (product_id) DO NOTHING`, ids, qty)
	if err != nil {
		return 0, classify(err)
	}
	return tag.RowsAffected(), nil
}

// Setup is the start-up path: the migrations, then the embedded seed stock. It returns how many migrations
// were applied and how many stock rows were inserted.
func Setup(ctx context.Context, pool *pgxpool.Pool) (applied int, seeded int64, err error) {
	res, err := Migrate(ctx, pool)
	if err != nil {
		return 0, 0, fmt.Errorf("migrate: %w", err)
	}
	seeded, err = Seed(ctx, pool, data.Stock)
	if err != nil {
		return len(res), 0, fmt.Errorf("seed: %w", err)
	}
	return len(res), seeded, nil
}
