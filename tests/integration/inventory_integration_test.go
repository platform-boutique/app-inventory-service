//go:build integration

// Integration tests against a real Postgres. Run with:
//
//	TEST_DATABASE_URL=postgres://inv:inv@127.0.0.1:55437/postgres?sslmode=disable \
//	  go test -race -tags integration ./tests/integration/
//
// Every test creates (and drops) its own database, so they need a user that may CREATE DATABASE.
package integration

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"inventory-service/data"
	"inventory-service/internal/inventory"
	pb "inventory-service/proto"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

var ctx = context.Background()

// freshDB creates an empty database and returns its DSN.
func freshDB(t *testing.T) string {
	t.Helper()
	admin := os.Getenv("TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	name := fmt.Sprintf("inv_test_%d", time.Now().UnixNano())
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), admin)
		if err != nil {
			return
		}
		defer func() { _ = c.Close(context.Background()) }()
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

func newPool(t *testing.T, dsn string, maxConns int32) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// Time in these tests: expiry is decided by the process's wall clock, and the wall clock of the machine these
// tests were developed on is not stable (it jumps +58 s for ~60 ms every ~5 s and steps back by up to 0.5 s; a
// hold made inside such a window had expires_at up to 55 s away, which made tests with real sleeps and
// 100-300 ms TTLs fail about 1 run in 5). So every test runs on an injected fixed clock (setup's default) and
// moves time with Advance, never with time.Sleep; the few waits that remain only wait for background
// goroutines, with generous deadlines.
//
// clock is a settable clock; with a zero value it follows the real time.
type clock struct {
	mu    sync.Mutex
	t     time.Time
	fixed bool
}

func fixedClock(t time.Time) *clock { return &clock{t: t, fixed: true} }
func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.fixed {
		return time.Now()
	}
	return c.t
}
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type env struct {
	svc  *inventory.Service
	pool *pgxpool.Pool
	logs *safeBuf
	dsn  string
	clk  *clock
}

// testStart is the fixed time every test starts at (unless it passes its own clock).
var testStart = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

// safeBuf is a log sink that is safe for the concurrent writers of the tests.
type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *safeBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type options struct {
	ttl      time.Duration
	clk      *clock
	maxConns int32
}

// setup migrates and seeds a fresh database and returns a service on it.
func setup(t *testing.T, o options) *env {
	t.Helper()
	dsn := freshDB(t)
	if o.clk == nil {
		o.clk = fixedClock(testStart)
	}
	if o.maxConns == 0 {
		o.maxConns = 30
	}
	p := newPool(t, dsn, o.maxConns)
	if _, _, err := inventory.Setup(ctx, p); err != nil {
		t.Fatal(err)
	}
	return newEnv(t, p, dsn, o)
}

func newEnv(t *testing.T, p *pgxpool.Pool, dsn string, o options) *env {
	t.Helper()
	logs := &safeBuf{}
	l := logrus.New()
	l.SetLevel(logrus.DebugLevel)
	l.Out = logs
	cfg := inventory.Config{ReservationTTL: o.ttl}
	if o.clk != nil {
		cfg.Now = o.clk.Now
	}
	svc := inventory.NewService(inventory.NewPGStore(p), l, cfg)
	svc.SetReady(true)
	t.Cleanup(svc.Wait)
	return &env{svc: svc, pool: p, logs: logs, dsn: dsn, clk: o.clk}
}

func code(err error) codes.Code { return status.Code(err) }

// setStock sets the stock of test products (inserting them if needed).
func (e *env) setStock(t *testing.T, kv ...any) {
	t.Helper()
	for i := 0; i < len(kv); i += 2 {
		if _, err := e.pool.Exec(ctx,
			`INSERT INTO stock (product_id, available) VALUES ($1, $2)
			 ON CONFLICT (product_id) DO UPDATE SET available = EXCLUDED.available`, kv[i], kv[i+1]); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *env) dbStock(t *testing.T, id string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(ctx, `SELECT available FROM stock WHERE product_id = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *env) dbStatus(t *testing.T, order string) string {
	t.Helper()
	var s string
	err := e.pool.QueryRow(ctx, `SELECT status FROM reservations WHERE order_id = $1`, order).Scan(&s)
	if err == pgx.ErrNoRows {
		return "(none)"
	}
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (e *env) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func lines(kv ...any) []*pb.ReserveItem {
	var out []*pb.ReserveItem
	for i := 0; i < len(kv); i += 2 {
		out = append(out, &pb.ReserveItem{ProductId: kv[i].(string), Quantity: int32(kv[i+1].(int))})
	}
	return out
}

func (e *env) reserve(order string, kv ...any) (*pb.ReserveResponse, error) {
	return e.svc.Reserve(ctx, &pb.ReserveRequest{OrderId: order, Items: lines(kv...)})
}
func (e *env) commit(order string) error {
	_, err := e.svc.Commit(ctx, &pb.CommitRequest{OrderId: order})
	return err
}
func (e *env) release(order string) error {
	_, err := e.svc.Release(ctx, &pb.ReleaseRequest{OrderId: order})
	return err
}
func (e *env) level(t *testing.T, id string) (int32, bool) {
	t.Helper()
	r, err := e.svc.GetStock(ctx, &pb.GetStockRequest{ProductIds: []string{id}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Levels) == 0 {
		return 0, false
	}
	return r.Levels[0].Available, true
}

// checkConservation asserts, for each product, available + held-or-sold quantities == initial, and that
// no stock is negative. This is the invariant that every race below must keep.
func (e *env) checkConservation(t *testing.T, initial map[string]int) {
	t.Helper()
	rows, err := e.pool.Query(ctx,
		`SELECT s.product_id, s.available,
		        COALESCE((SELECT sum(i.quantity) FROM reservation_items i JOIN reservations r USING (order_id)
		                   WHERE i.product_id = s.product_id AND r.status IN ('RESERVED','COMMITTED')), 0)
		   FROM stock s WHERE s.product_id = ANY($1)`, keys(initial))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var id string
		var avail, held int
		if err := rows.Scan(&id, &avail, &held); err != nil {
			t.Fatal(err)
		}
		seen++
		if avail < 0 {
			t.Errorf("%s: available %d is negative", id, avail)
		}
		if avail+held != initial[id] {
			t.Errorf("%s: available %d + held/sold %d != initial %d", id, avail, held, initial[id])
		}
	}
	if seen != len(initial) {
		t.Errorf("saw %d products, want %d", seen, len(initial))
	}
}

func keys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------------------------------

func TestMigrationsAndSeed(t *testing.T) {
	e := setup(t, options{})
	seed, err := inventory.ParseSeed(data.Stock)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.count(t, "stock"); got != len(seed) {
		t.Fatalf("stock rows = %d, want %d", got, len(seed))
	}
	for _, s := range seed[:5] {
		if got := e.dbStock(t, s.ProductID); got != int(s.Available) {
			t.Errorf("%s: %d, want %d", s.ProductID, got, s.Available)
		}
	}
	// a second run applies nothing and seeds nothing
	applied, seeded, err := inventory.Setup(ctx, e.pool)
	if err != nil || applied != 0 || seeded != 0 {
		t.Fatalf("second Setup: applied %d seeded %d err %v", applied, seeded, err)
	}
}

func TestSixConcurrentStartsApplyMigrationsOnce(t *testing.T) {
	dsn := freshDB(t)
	var wg sync.WaitGroup
	var applied, seeded atomic.Int64
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := pgxpool.New(ctx, dsn)
			if err != nil {
				errs <- err
				return
			}
			defer p.Close()
			a, s, err := inventory.Setup(ctx, p)
			if err != nil {
				errs <- err
				return
			}
			applied.Add(int64(a))
			seeded.Add(s)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	seed, _ := inventory.ParseSeed(data.Stock)
	if applied.Load() != 1 {
		t.Errorf("migrations applied %d times in total, want 1", applied.Load())
	}
	if seeded.Load() != int64(len(seed)) {
		t.Errorf("seeded %d rows in total, want %d", seeded.Load(), len(seed))
	}
	p := newPool(t, dsn, 2)
	var n int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM goose_db_version WHERE version_id = 1`).Scan(&n); err != nil || n != 1 {
		t.Errorf("goose_db_version rows for version 1: %d (%v)", n, err)
	}
}

func TestSeedingTwiceDoesNotResetLiveStock(t *testing.T) {
	e := setup(t, options{})
	seed, _ := inventory.ParseSeed(data.Stock)
	var pick inventory.SeedStock
	for _, s := range seed {
		if s.Available >= 10 {
			pick = s
			break
		}
	}
	if _, err := e.reserve("o1", pick.ProductID, 4); err != nil {
		t.Fatal(err)
	}
	if err := e.commit("o1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.reserve("o2", pick.ProductID, 3); err != nil { // stays RESERVED
		t.Fatal(err)
	}
	want := int(pick.Available) - 7
	if n, err := inventory.Seed(ctx, e.pool, data.Stock); err != nil || n != 0 {
		t.Fatalf("Seed inserted %d rows (err %v), want 0", n, err)
	}
	if _, _, err := inventory.Setup(ctx, e.pool); err != nil {
		t.Fatal(err)
	}
	if got := e.dbStock(t, pick.ProductID); got != want {
		t.Fatalf("%s: %d after seeding again, want %d (live stock was reset)", pick.ProductID, got, want)
	}
	// a product that is new in the seed is added without touching the others
	extra := []byte(`[{"product_id":"brand-new-1","available":9}]`)
	if n, err := inventory.Seed(ctx, e.pool, extra); err != nil || n != 1 {
		t.Fatalf("Seed of a new product: %d %v", n, err)
	}
	if got := e.dbStock(t, pick.ProductID); got != want {
		t.Fatalf("stock changed by seeding a new product: %d", got)
	}
}

func TestLifecycleExactAvailability(t *testing.T) {
	e := setup(t, options{})
	e.setStock(t, "a", 10, "b", 5)
	check := func(step string, a, b int) {
		t.Helper()
		if ga, gb := e.dbStock(t, "a"), e.dbStock(t, "b"); ga != a || gb != b {
			t.Fatalf("%s: a=%d b=%d, want a=%d b=%d", step, ga, gb, a, b)
		}
		if la, _ := e.level(t, "a"); int(la) != a {
			t.Fatalf("%s: GetStock a = %d, want %d", step, la, a)
		}
	}
	check("start", 10, 5)
	if r, err := e.reserve("o1", "a", 3, "b", 2); err != nil || !r.Reserved {
		t.Fatal(r, err)
	}
	e.clk.Advance(time.Second)
	check("reserved o1", 7, 3)
	if r, err := e.reserve("o2", "a", 7, "b", 3); err != nil || !r.Reserved {
		t.Fatal(r, err)
	}
	check("reserved o2 (everything)", 0, 0)
	if e.dbStatus(t, "o1") != "RESERVED" {
		t.Fatal("o1 not RESERVED")
	}
	if err := e.commit("o1"); err != nil {
		t.Fatal(err)
	}
	check("committed o1", 0, 0)
	if e.dbStatus(t, "o1") != "COMMITTED" {
		t.Fatal("o1 not COMMITTED")
	}
	if err := e.release("o2"); err != nil {
		t.Fatal(err)
	}
	check("released o2", 7, 3)
	if e.dbStatus(t, "o2") != "RELEASED" {
		t.Fatal("o2 not RELEASED")
	}
	// the stock can be taken again after the release
	if r, err := e.reserve("o3", "a", 7, "b", 3); err != nil || !r.Reserved {
		t.Fatal(r, err)
	}
	check("reserved o3", 0, 0)
	var created, updated, expires time.Time
	if err := e.pool.QueryRow(ctx, `SELECT created_at, updated_at, expires_at FROM reservations WHERE order_id = 'o1'`).Scan(&created, &updated, &expires); err != nil {
		t.Fatal(err)
	}
	if !created.Equal(testStart) || !expires.Equal(testStart.Add(15*time.Minute)) || !updated.Equal(testStart.Add(time.Second)) {
		t.Errorf("timestamps: created %v updated %v expires %v, want %v, +1s, +15m", created, updated, expires, testStart)
	}
}

func TestShortagesListedAndNothingHeld(t *testing.T) {
	e := setup(t, options{})
	e.setStock(t, "a", 2, "b", 0, "c", 9, "d", 1)
	r, err := e.reserve("o1", "c", 1, "a", 3, "b", 1, "d", 1, "ghost", 4)
	if err != nil {
		t.Fatal(err)
	}
	if r.Reserved || len(r.Shortages) != 3 {
		t.Fatalf("Reserve = %v", r)
	}
	got := map[string][2]int32{}
	for _, s := range r.Shortages {
		got[s.ProductId] = [2]int32{s.Requested, s.Available}
	}
	want := map[string][2]int32{"a": {3, 2}, "b": {1, 0}, "ghost": {4, 0}}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("shortage %s = %v, want %v (all: %v)", id, got[id], w, got)
		}
	}
	for id, w := range map[string]int{"a": 2, "b": 0, "c": 9, "d": 1} {
		if g := e.dbStock(t, id); g != w {
			t.Errorf("%s = %d after a refused reserve, want %d (something was held)", id, g, w)
		}
	}
	if e.count(t, "reservations") != 0 || e.count(t, "reservation_items") != 0 {
		t.Error("a refused reserve left rows behind")
	}
	if e.dbStatus(t, "o1") != "(none)" {
		t.Error("reservation row for a refused reserve")
	}
}

func TestUnknownProductIsAShortageWithZero(t *testing.T) {
	e := setup(t, options{})
	r, err := e.reserve("o1", "no-such-product", 2)
	if err != nil || r.Reserved || len(r.Shortages) != 1 {
		t.Fatalf("%v %v", r, err)
	}
	if s := r.Shortages[0]; s.ProductId != "no-such-product" || s.Requested != 2 || s.Available != 0 {
		t.Fatalf("shortage = %v", s)
	}
	if _, ok := e.level(t, "no-such-product"); ok {
		t.Error("an unknown product must be missing from GetStock")
	}
}

func TestIdempotencyAndStateRules(t *testing.T) {
	e := setup(t, options{})
	e.setStock(t, "a", 10, "b", 10)
	if r, err := e.reserve("o1", "a", 1, "b", 2); err != nil || !r.Reserved {
		t.Fatal(r, err)
	}
	// replay of Reserve (also with the lines in another order)
	for i := 0; i < 3; i++ {
		if r, err := e.reserve("o1", "b", 2, "a", 1); err != nil || !r.Reserved {
			t.Fatalf("replay %d: %v %v", i, r, err)
		}
	}
	if e.dbStock(t, "a") != 9 || e.dbStock(t, "b") != 8 {
		t.Fatal("replays took stock again")
	}
	// same id, other items
	for _, items := range [][]any{{"a", 2, "b", 2}, {"a", 1}, {"a", 1, "b", 2, "c", 1}, {"a", 1, "x", 2}} {
		if _, err := e.reserve("o1", items...); code(err) != codes.AlreadyExists {
			t.Errorf("items %v: %v, want ALREADY_EXISTS", items, err)
		}
	}
	// Commit twice
	if err := e.commit("o1"); err != nil {
		t.Fatal(err)
	}
	if err := e.commit("o1"); err != nil {
		t.Fatalf("second commit: %v", err)
	}
	// replay of Reserve after Commit is still reserved=true
	if r, err := e.reserve("o1", "a", 1, "b", 2); err != nil || !r.Reserved {
		t.Fatalf("replay after commit: %v %v", r, err)
	}
	// Release after Commit
	if err := e.release("o1"); code(err) != codes.FailedPrecondition {
		t.Fatalf("Release after Commit: %v", err)
	}
	if e.dbStatus(t, "o1") != "COMMITTED" || e.dbStock(t, "a") != 9 || e.dbStock(t, "b") != 8 {
		t.Fatal("a refused Release changed something")
	}

	// Release twice, Commit after Release, Reserve of a released id
	if _, err := e.reserve("o2", "a", 4); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := e.release("o2"); err != nil {
			t.Fatalf("release %d: %v", i, err)
		}
	}
	if e.dbStock(t, "a") != 9 {
		t.Fatalf("a = %d after release x3, want 9 (returned more than once?)", e.dbStock(t, "a"))
	}
	if err := e.commit("o2"); code(err) != codes.FailedPrecondition {
		t.Fatalf("Commit after Release: %v", err)
	}
	if _, err := e.reserve("o2", "a", 4); code(err) != codes.FailedPrecondition {
		t.Fatalf("Reserve of a released id: %v", err)
	}
	if e.dbStock(t, "a") != 9 {
		t.Fatal("a refused Reserve took stock")
	}

	// unknown ids
	if err := e.commit("never"); code(err) != codes.NotFound {
		t.Fatalf("Commit of unknown: %v", err)
	}
	if err := e.release("never"); err != nil {
		t.Fatalf("Release of unknown: %v", err)
	}
	if e.dbStatus(t, "never") != "(none)" {
		t.Fatal("Release of an unknown id created a row")
	}
}

func TestExpiryReturnsStockAndRefusesCommit(t *testing.T) {
	e := setup(t, options{ttl: 300 * time.Millisecond})
	e.setStock(t, "a", 5, "b", 5)
	for _, o := range []string{"lazy-get", "lazy-commit", "lazy-reserve", "sweep"} {
		if _, err := e.reserve(o, "a", 1, "b", 1); err != nil {
			t.Fatal(err)
		}
	}
	if e.dbStock(t, "a") != 1 {
		t.Fatal("holds not taken")
	}
	e.clk.Advance(400 * time.Millisecond)
	if e.dbStock(t, "a") != 1 {
		t.Fatal("expiry must be lazy: nothing touched the holds yet")
	}
	// Commit of an expired hold: refused, and the stock comes back in the same call
	if err := e.commit("lazy-commit"); code(err) != codes.FailedPrecondition {
		t.Fatalf("Commit after expiry: %v", err)
	}
	if e.dbStatus(t, "lazy-commit") != "RELEASED" || e.dbStock(t, "a") != 2 {
		t.Fatalf("after Commit: status %s, a=%d (want RELEASED and 2)", e.dbStatus(t, "lazy-commit"), e.dbStock(t, "a"))
	}
	// GetStock releases every expired hold on the products it is asked for
	if n, _ := e.level(t, "a"); n != 5 {
		t.Fatalf("GetStock after expiry = %d, want 5", n)
	}
	for _, o := range []string{"lazy-get", "lazy-reserve", "sweep"} {
		if e.dbStatus(t, o) != "RELEASED" {
			t.Errorf("%s: %s, want RELEASED", o, e.dbStatus(t, o))
		}
	}
	if n, _ := e.level(t, "b"); n != 5 {
		t.Fatalf("b = %d, want 5", n)
	}
	// later Commit / Reserve of those ids fail, Release is fine
	if err := e.commit("lazy-get"); code(err) != codes.FailedPrecondition {
		t.Fatalf("later Commit: %v", err)
	}
	if _, err := e.reserve("lazy-get", "a", 1, "b", 1); code(err) != codes.FailedPrecondition {
		t.Fatalf("later Reserve: %v", err)
	}
	if err := e.release("lazy-get"); err != nil {
		t.Fatalf("later Release: %v", err)
	}
	if e.dbStock(t, "a") != 5 || e.dbStock(t, "b") != 5 {
		t.Fatal("stock returned more than once")
	}

	// Reserve of the stock an expired hold had: succeeds because Reserve releases it first
	if _, err := e.reserve("holder", "a", 5); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(400 * time.Millisecond)
	if r, err := e.reserve("taker", "a", 5); err != nil || !r.Reserved {
		t.Fatalf("Reserve over an expired hold: %v %v", r, err)
	}
	// Reserve with the order's OWN expired hold: refused, stock back
	e.clk.Advance(400 * time.Millisecond)
	if _, err := e.reserve("taker", "a", 5); code(err) != codes.FailedPrecondition {
		t.Fatalf("replay of an expired reservation: %v", err)
	}
	if e.dbStock(t, "a") != 5 {
		t.Fatalf("a = %d, want 5", e.dbStock(t, "a"))
	}

	// a committed reservation never expires
	if _, err := e.reserve("sold", "a", 2); err != nil {
		t.Fatal(err)
	}
	if err := e.commit("sold"); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(400 * time.Millisecond)
	if n, err := e.svc.SweepOnce(ctx); err != nil || n != 0 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	if n, _ := e.level(t, "a"); n != 3 {
		t.Fatalf("a = %d, want 3 (committed stays sold)", n)
	}
}

func TestBackgroundSweeperReturnsStockWithoutAnyCall(t *testing.T) {
	e := setup(t, options{ttl: 100 * time.Millisecond})
	e.setStock(t, "a", 3)
	if _, err := e.reserve("o1", "a", 3); err != nil {
		t.Fatal(err)
	}
	// a second service with a short sweep interval runs the sweeper on the same database and the same
	// (injected) clock, so "expired" is decided by Advance, not by how long the machine takes
	l := logrus.New()
	l.Out = &safeBuf{}
	svc := inventory.NewService(inventory.NewPGStore(e.pool), l, inventory.Config{ReservationTTL: time.Minute, SweepEvery: 20 * time.Millisecond, Now: e.clk.Now})
	svc.SetReady(true)
	sctx, stop := context.WithCancel(ctx)
	svc.RunSweeper(sctx)
	time.Sleep(200 * time.Millisecond) // about ten sweeps happen; the hold is not expired, so none may touch it
	if e.dbStock(t, "a") != 0 || e.dbStatus(t, "o1") != "RESERVED" {
		stop()
		svc.Wait()
		t.Fatalf("swept before expiry: a=%d status=%s", e.dbStock(t, "a"), e.dbStatus(t, "o1"))
	}
	e.clk.Advance(200 * time.Millisecond) // now it is expired
	deadline := time.Now().Add(30 * time.Second)
	for e.dbStock(t, "a") != 3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	svc.Wait()
	if e.dbStock(t, "a") != 3 || e.dbStatus(t, "o1") != "RELEASED" {
		t.Fatalf("a=%d status=%s", e.dbStock(t, "a"), e.dbStatus(t, "o1"))
	}
}

func TestOversell200ParallelReservesOfOneUnitAgainstStock50(t *testing.T) {
	e := setup(t, options{maxConns: 40})
	e.setStock(t, "hot", 50)
	var wg sync.WaitGroup
	var yes, no, failed atomic.Int64
	start := make(chan struct{})
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			r, err := e.reserve(fmt.Sprintf("race-%d", i), "hot", 1)
			switch {
			case err != nil:
				failed.Add(1)
			case r.Reserved:
				yes.Add(1)
			default:
				no.Add(1)
				if len(r.Shortages) != 1 || r.Shortages[0].Requested != 1 {
					t.Errorf("shortage = %v", r.Shortages)
				}
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if yes.Load() != 50 || no.Load() != 150 || failed.Load() != 0 {
		t.Fatalf("reserved=%d refused=%d errors=%d, want 50/150/0", yes.Load(), no.Load(), failed.Load())
	}
	if got := e.dbStock(t, "hot"); got != 0 {
		t.Fatalf("available = %d, want 0", got)
	}
	var held int
	if err := e.pool.QueryRow(ctx, `SELECT COALESCE(sum(quantity),0) FROM reservation_items WHERE product_id = 'hot'`).Scan(&held); err != nil || held != 50 {
		t.Fatalf("held = %d (%v), want 50", held, err)
	}
	if e.count(t, "reservations") != 50 {
		t.Fatalf("reservations = %d, want 50", e.count(t, "reservations"))
	}
	e.checkConservation(t, map[string]int{"hot": 50})
}

func TestMultiLineReservationsInOppositeOrderDoNotDeadlock(t *testing.T) {
	e := setup(t, options{maxConns: 40})
	ids := []string{"p1", "p2", "p3", "p4", "p5"}
	initial := map[string]int{}
	for _, id := range ids {
		e.setStock(t, id, 1000)
		initial[id] = 1000
	}
	forward := lines("p1", 1, "p2", 1, "p3", 1, "p4", 1, "p5", 1)
	backward := lines("p5", 1, "p4", 1, "p3", 1, "p2", 1, "p1", 1)
	store := inventory.NewPGStore(e.pool)

	run := func(name string, reserve func(order string, items []*pb.ReserveItem) (bool, error)) {
		var wg sync.WaitGroup
		var okN, errN atomic.Int64
		done := make(chan struct{})
		go func() {
			for i := 0; i < 200; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					items := forward
					if i%2 == 1 {
						items = backward
					}
					ok, err := reserve(fmt.Sprintf("%s-%d", name, i), items)
					if err != nil {
						errN.Add(1)
						t.Errorf("%s-%d: %v", name, i, err)
					} else if ok {
						okN.Add(1)
					}
				}(i)
			}
			wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(60 * time.Second):
			t.Fatalf("%s: deadlock (not finished after 60 s)", name)
		}
		if okN.Load() != 200 || errN.Load() != 0 {
			t.Fatalf("%s: reserved %d, errors %d, want 200/0", name, okN.Load(), errN.Load())
		}
	}
	// through the service (it sorts the lines)...
	run("svc", func(order string, items []*pb.ReserveItem) (bool, error) {
		r, err := e.svc.Reserve(ctx, &pb.ReserveRequest{OrderId: order, Items: items})
		if err != nil {
			return false, err
		}
		return r.Reserved, nil
	})
	// ... and straight into the store with the lines in the caller's (unsorted) order: the row locks are
	// taken in sorted order by the store itself
	run("store", func(order string, items []*pb.ReserveItem) (bool, error) {
		in := make([]inventory.Item, len(items))
		for i, it := range items {
			in[i] = inventory.Item{ProductID: it.ProductId, Quantity: it.Quantity}
		}
		r, err := store.Reserve(ctx, order, in, e.clk.Now(), time.Minute)
		return r.Reserved, err
	})
	for _, id := range ids {
		if got := e.dbStock(t, id); got != 600 {
			t.Errorf("%s = %d, want 600", id, got)
		}
	}
	e.checkConservation(t, initial)
}

func TestMixedConcurrentOperationsKeepTheBooksBalanced(t *testing.T) {
	e := setup(t, options{maxConns: 40})
	initial := map[string]int{"m1": 30, "m2": 20, "m3": 10}
	for id, n := range initial {
		e.setStock(t, id, n)
	}
	var wg sync.WaitGroup
	for i := 0; i < 150; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			o := fmt.Sprintf("mix-%d", i)
			var items []any
			switch i % 3 {
			case 0:
				items = []any{"m1", 1 + i%3, "m2", 2}
			case 1:
				items = []any{"m3", 1, "m2", 1, "m1", 2}
			default:
				items = []any{"m2", 3}
			}
			r, err := e.reserve(o, items...)
			if err != nil {
				t.Errorf("reserve %s: %v", o, err)
				return
			}
			if !r.Reserved {
				return
			}
			switch i % 4 {
			case 0:
				if err := e.commit(o); err != nil {
					t.Errorf("commit %s: %v", o, err)
				}
			case 1:
				if err := e.release(o); err != nil {
					t.Errorf("release %s: %v", o, err)
				}
			case 2: // replay racing with a release of the same order
				wg.Add(1) // wg is still above zero here (this goroutine is counted)
				go func() { defer wg.Done(); _ = e.release(o) }()
				_, _ = e.reserve(o, items...)
			}
		}(i)
	}
	wg.Wait() // includes the releases that race with the replays
	e.checkConservation(t, initial)
}

func TestConcurrentReplaysOfOneOrderTakeStockOnce(t *testing.T) {
	e := setup(t, options{maxConns: 40})
	e.setStock(t, "a", 10, "b", 10)
	var wg sync.WaitGroup
	var reserved, other atomic.Int64
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := e.reserve("same", "a", 2, "b", 3)
			if err == nil && r.Reserved {
				reserved.Add(1)
			} else {
				other.Add(1)
				t.Errorf("replay: %v %v", r, err)
			}
		}()
	}
	wg.Wait()
	if reserved.Load() != 30 || e.dbStock(t, "a") != 8 || e.dbStock(t, "b") != 7 || e.count(t, "reservations") != 1 {
		t.Fatalf("reserved %d, a=%d b=%d, reservations=%d", reserved.Load(), e.dbStock(t, "a"), e.dbStock(t, "b"), e.count(t, "reservations"))
	}
	// the same order id with 2 different item sets at once: one wins, the rest see the winner's items
	var winA, winB, conflicts atomic.Int64
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			items := []any{"a", 1}
			if i%2 == 1 {
				items = []any{"b", 1}
			}
			r, err := e.reserve("dup", items...)
			switch {
			case err == nil && r.Reserved && i%2 == 0:
				winA.Add(1)
			case err == nil && r.Reserved:
				winB.Add(1)
			case code(err) == codes.AlreadyExists:
				conflicts.Add(1)
			default:
				t.Errorf("unexpected: %v %v", r, err)
			}
		}(i)
	}
	wg.Wait()
	if (winA.Load() == 0) == (winB.Load() == 0) || winA.Load()+winB.Load()+conflicts.Load() != 30 {
		t.Fatalf("winA %d winB %d conflicts %d: exactly one item set must win", winA.Load(), winB.Load(), conflicts.Load())
	}
	e.checkConservation(t, map[string]int{"a": 10, "b": 10})
}

func TestConcurrentCommitVersusSweepNeverCommitsWithReturnedStock(t *testing.T) {
	const orders = 200
	t0 := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	reserveClock := fixedClock(t0)
	e := setup(t, options{ttl: time.Minute, clk: reserveClock, maxConns: 40})
	initial := map[string]int{"x": orders, "y": orders * 2}
	e.setStock(t, "x", orders, "y", orders*2)
	for i := 0; i < orders; i++ {
		if r, err := e.reserve(fmt.Sprintf("cs-%d", i), "x", 1, "y", 2); err != nil || !r.Reserved {
			t.Fatal(r, err)
		}
	}
	// Two services on the same database: the committer's clock is one second BEFORE the expiry, the sweeper's
	// one second AFTER it. Whoever takes the row locks first wins; the other must then see the new state.
	// Each side has its own small pool, so both make progress at the same time instead of one starving the other.
	committer := newEnv(t, newPool(t, e.dsn, 4), e.dsn, options{ttl: time.Minute, clk: fixedClock(t0.Add(time.Minute - time.Second))})
	sweeper := newEnv(t, newPool(t, e.dsn, 8), e.dsn, options{ttl: time.Minute, clk: fixedClock(t0.Add(time.Minute + time.Second))})

	var wg sync.WaitGroup
	commitErr := make([]error, orders)
	start := make(chan struct{})
	for i := 0; i < orders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			commitErr[i] = committer.commit(fmt.Sprintf("cs-%d", i))
		}(i)
	}
	var swept atomic.Int64
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			n, err := sweeper.svc.SweepOnce(ctx)
			if err != nil {
				t.Errorf("sweep: %v", err)
			}
			swept.Add(int64(n))
		}()
	}
	close(start)
	wg.Wait()

	committed, released := 0, 0
	for i := 0; i < orders; i++ {
		o := fmt.Sprintf("cs-%d", i)
		st := e.dbStatus(t, o)
		switch {
		case st == "COMMITTED" && commitErr[i] == nil:
			committed++
		case st == "RELEASED" && code(commitErr[i]) == codes.FailedPrecondition:
			released++
		default:
			t.Errorf("%s: status %s but Commit returned %v", o, st, commitErr[i])
		}
	}
	if int64(released) != swept.Load() {
		t.Errorf("sweeps released %d, but %d reservations are RELEASED", swept.Load(), released)
	}
	t.Logf("commit won %d, sweep won %d", committed, released)
	e.checkConservation(t, initial)
	if got := e.dbStock(t, "x"); got != released {
		t.Errorf("x = %d, want %d (one unit back per released reservation)", got, released)
	}
	// the sweep is exhaustive: nothing is left RESERVED and expired
	if n, err := sweeper.svc.SweepOnce(ctx); err != nil || n != 0 {
		t.Errorf("a second sweep released %d (%v)", n, err)
	}
}

func TestCheckConstraints(t *testing.T) {
	e := setup(t, options{})
	e.setStock(t, "a", 1)
	for name, q := range map[string]string{
		"negative stock":      `UPDATE stock SET available = -1 WHERE product_id = 'a'`,
		"negative insert":     `INSERT INTO stock VALUES ('neg', -5)`,
		"empty product id":    `INSERT INTO stock VALUES ('', 1)`,
		"bad status":          `INSERT INTO reservations VALUES ('o', 'WEIRD', now(), now(), now() + interval '1 minute')`,
		"expiry before start": `INSERT INTO reservations VALUES ('o', 'RESERVED', now(), now(), now() - interval '1 minute')`,
	} {
		if _, err := e.pool.Exec(ctx, q); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO reservations VALUES ('o', 'RESERVED', now(), now(), now() + interval '1 minute')`); err != nil {
		t.Fatal(err)
	}
	for name, q := range map[string]string{
		"quantity 0":      `INSERT INTO reservation_items VALUES ('o', 'a', 0)`,
		"quantity 1001":   `INSERT INTO reservation_items VALUES ('o', 'a', 1001)`,
		"unknown product": `INSERT INTO reservation_items VALUES ('o', 'ghost', 1)`,
		"unknown order":   `INSERT INTO reservation_items VALUES ('nope', 'a', 1)`,
	} {
		if _, err := e.pool.Exec(ctx, q); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDatabaseDownIsUnavailableWithoutSQLDetails(t *testing.T) {
	// a pool to a port nobody listens on
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	dsn := fmt.Sprintf("postgres://inv:topsecretpw@127.0.0.1:%d/inv?sslmode=disable&connect_timeout=1", port)
	p := newPool(t, dsn, 2)
	e := newEnv(t, p, dsn, options{})
	check := func(name string, err error) {
		t.Helper()
		if code(err) != codes.Unavailable {
			t.Errorf("%s: code %v (%v), want UNAVAILABLE", name, code(err), err)
			return
		}
		msg := status.Convert(err).Message()
		for _, bad := range []string{"127.0.0.1", "topsecretpw", "dial", "refused", "pq:", "SQLSTATE", "stock", "reservations"} {
			if strings.Contains(msg, bad) {
				t.Errorf("%s: message %q leaks %q", name, msg, bad)
			}
		}
	}
	_, err = e.svc.GetStock(ctx, &pb.GetStockRequest{ProductIds: []string{"a"}})
	check("GetStock", err)
	_, err = e.reserve("o1", "a", 1)
	check("Reserve", err)
	check("Commit", e.commit("o1"))
	check("Release", e.release("o1"))
	if strings.Contains(e.logs.String(), "topsecretpw") {
		t.Error("the password is in the log")
	}
}

func TestPoolClosedIsUnavailable(t *testing.T) {
	e := setup(t, options{})
	e.pool.Close()
	_, err := e.reserve("o1", "a", 1)
	if code(err) != codes.Unavailable {
		t.Fatalf("code %v (%v), want UNAVAILABLE", code(err), err)
	}
}

func TestLogsOnlyOrderIDsAndCounts(t *testing.T) {
	e := setup(t, options{})
	e.setStock(t, "very-secret-sku", 5)
	_, _ = e.reserve("log-1", "very-secret-sku", 2)
	_, _ = e.reserve("log-2", "very-secret-sku", 9)
	_ = e.commit("log-1")
	_ = e.release("log-2")
	_ = e.release("log-1")
	out := e.logs.String()
	if !strings.Contains(out, "order_id=log-1") || !strings.Contains(out, "reserved=false shortages=1") {
		t.Fatalf("control failed: the log does not show what it should:\n%s", out)
	}
	if strings.Contains(out, "very-secret-sku") {
		t.Fatalf("a product id is in the log:\n%s", out)
	}
}

func TestOverGRPC(t *testing.T) {
	e := setup(t, options{})
	e.setStock(t, "a", 3)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	pb.RegisterInventoryServiceServer(srv, e.svc)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(l.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	c := pb.NewInventoryServiceClient(conn)

	r, err := c.Reserve(ctx, &pb.ReserveRequest{OrderId: "g1", Items: lines("a", 2)})
	if err != nil || !r.Reserved {
		t.Fatal(r, err)
	}
	r, err = c.Reserve(ctx, &pb.ReserveRequest{OrderId: "g2", Items: lines("a", 2, "ghost", 1)})
	if err != nil || r.Reserved || len(r.Shortages) != 2 {
		t.Fatalf("%v %v", r, err)
	}
	gs, err := c.GetStock(ctx, &pb.GetStockRequest{ProductIds: []string{"ghost", "a"}})
	if err != nil || len(gs.Levels) != 1 || gs.Levels[0].Available != 1 {
		t.Fatalf("%v %v", gs, err)
	}
	if _, err := c.Commit(ctx, &pb.CommitRequest{OrderId: "g1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Release(ctx, &pb.ReleaseRequest{OrderId: "g1"}); code(err) != codes.FailedPrecondition {
		t.Fatalf("Release after Commit: %v", err)
	}
	if _, err := c.Commit(ctx, &pb.CommitRequest{OrderId: "nope"}); code(err) != codes.NotFound {
		t.Fatalf("Commit unknown: %v", err)
	}
	if _, err := c.Reserve(ctx, &pb.ReserveRequest{OrderId: "g3", Items: lines("a", 0)}); code(err) != codes.InvalidArgument {
		t.Fatalf("quantity 0: %v", err)
	}
	if _, err := c.Reserve(ctx, &pb.ReserveRequest{OrderId: "g1", Items: lines("a", 1)}); code(err) != codes.AlreadyExists {
		t.Fatalf("other items: %v", err)
	}
}
