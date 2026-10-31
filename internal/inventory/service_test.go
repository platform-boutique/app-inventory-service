package inventory

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	pb "inventory-service/proto"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var ctx = context.Background()

func code(err error) codes.Code { return status.Code(err) }

func reserve(env *testEnv, order string, kv ...any) (*pb.ReserveResponse, error) {
	return env.svc.Reserve(ctx, &pb.ReserveRequest{OrderId: order, Items: items(kv...)})
}

func levels(t *testing.T, env *testEnv, ids ...string) map[string]int32 {
	t.Helper()
	r, err := env.svc.GetStock(ctx, &pb.GetStockRequest{ProductIds: ids})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int32{}
	for _, l := range r.Levels {
		out[l.ProductId] = l.Available
	}
	return out
}

func TestLifecycle(t *testing.T) {
	env := newTestService(t, map[string]int32{"a": 10, "b": 5})
	r, err := reserve(env, "o1", "a", 3, "b", 5)
	if err != nil || !r.Reserved || len(r.Shortages) != 0 {
		t.Fatalf("Reserve = %v, %v", r, err)
	}
	if got := levels(t, env, "a", "b"); got["a"] != 7 || got["b"] != 0 {
		t.Fatalf("after reserve: %v", got)
	}
	if _, err := env.svc.Commit(ctx, &pb.CommitRequest{OrderId: "o1"}); err != nil {
		t.Fatal(err)
	}
	if got := levels(t, env, "a", "b"); got["a"] != 7 || got["b"] != 0 {
		t.Fatalf("after commit: %v", got)
	}
	if _, err := env.svc.Release(ctx, &pb.ReleaseRequest{OrderId: "o1"}); code(err) != codes.FailedPrecondition {
		t.Fatalf("Release after Commit: %v", err)
	}
	if _, err := reserve(env, "o2", "a", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.Release(ctx, &pb.ReleaseRequest{OrderId: "o2"}); err != nil {
		t.Fatal(err)
	}
	if got := levels(t, env, "a"); got["a"] != 7 {
		t.Fatalf("after release: %v", got)
	}
}

func TestShortagesListEveryProductAndHoldNothing(t *testing.T) {
	env := newTestService(t, map[string]int32{"a": 2, "b": 0, "c": 9})
	r, err := reserve(env, "o1", "c", 1, "a", 3, "b", 1, "ghost", 4)
	if err != nil {
		t.Fatal(err)
	}
	if r.Reserved || len(r.Shortages) != 3 {
		t.Fatalf("Reserve = %v", r)
	}
	want := map[string][2]int32{"a": {3, 2}, "b": {1, 0}, "ghost": {4, 0}}
	for _, s := range r.Shortages {
		if w, ok := want[s.ProductId]; !ok || s.Requested != w[0] || s.Available != w[1] {
			t.Errorf("unexpected shortage %v", s)
		}
	}
	if got := levels(t, env, "a", "b", "c"); got["a"] != 2 || got["b"] != 0 || got["c"] != 9 {
		t.Fatalf("something was held: %v", got)
	}
	// nothing is stored for the order: the same id can be reserved later with other items
	if r, err := reserve(env, "o1", "c", 2); err != nil || !r.Reserved {
		t.Fatalf("second try: %v %v", r, err)
	}
}

func TestIdempotentReplayAndConflicts(t *testing.T) {
	env := newTestService(t, map[string]int32{"a": 10, "b": 10})
	if r, err := reserve(env, "o1", "a", 1, "b", 2); err != nil || !r.Reserved {
		t.Fatal(r, err)
	}
	// replay with the lines in the other order: still reserved, nothing taken twice
	if r, err := reserve(env, "o1", "b", 2, "a", 1); err != nil || !r.Reserved {
		t.Fatalf("replay: %v %v", r, err)
	}
	if got := levels(t, env, "a", "b"); got["a"] != 9 || got["b"] != 8 {
		t.Fatalf("replay changed stock: %v", got)
	}
	if _, err := reserve(env, "o1", "a", 2, "b", 2); code(err) != codes.AlreadyExists {
		t.Fatalf("other quantity: %v", err)
	}
	if _, err := reserve(env, "o1", "a", 1); code(err) != codes.AlreadyExists {
		t.Fatalf("fewer lines: %v", err)
	}
	// replay after commit is still true
	if _, err := env.svc.Commit(ctx, &pb.CommitRequest{OrderId: "o1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.Commit(ctx, &pb.CommitRequest{OrderId: "o1"}); err != nil {
		t.Fatalf("second commit: %v", err)
	}
	if r, err := reserve(env, "o1", "a", 1, "b", 2); err != nil || !r.Reserved {
		t.Fatalf("replay after commit: %v %v", r, err)
	}
	if got := levels(t, env, "a", "b"); got["a"] != 9 || got["b"] != 8 {
		t.Fatalf("stock moved: %v", got)
	}
}

func TestReleaseRules(t *testing.T) {
	env := newTestService(t, map[string]int32{"a": 10})
	if _, err := env.svc.Release(ctx, &pb.ReleaseRequest{OrderId: "never-seen"}); err != nil {
		t.Fatalf("unknown order: %v", err)
	}
	if _, err := reserve(env, "o1", "a", 4); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ { // Release after Release is fine and returns the stock once
		if _, err := env.svc.Release(ctx, &pb.ReleaseRequest{OrderId: "o1"}); err != nil {
			t.Fatalf("release %d: %v", i, err)
		}
	}
	if got := levels(t, env, "a"); got["a"] != 10 {
		t.Fatalf("stock = %v, want 10", got)
	}
	if _, err := env.svc.Commit(ctx, &pb.CommitRequest{OrderId: "o1"}); code(err) != codes.FailedPrecondition {
		t.Fatalf("Commit after Release: %v", err)
	}
	if _, err := reserve(env, "o1", "a", 4); code(err) != codes.FailedPrecondition {
		t.Fatalf("Reserve of a released order id: %v", err)
	}
	if _, err := env.svc.Commit(ctx, &pb.CommitRequest{OrderId: "unknown"}); code(err) != codes.NotFound {
		t.Fatalf("Commit of unknown order: %v", err)
	}
}

func TestExpiry(t *testing.T) {
	env := newTestService(t, map[string]int32{"a": 3})
	if _, err := reserve(env, "o1", "a", 3); err != nil {
		t.Fatal(err)
	}
	env.clock.Advance(14*time.Minute + 59*time.Second)
	if got := levels(t, env, "a"); got["a"] != 0 {
		t.Fatalf("before expiry: %v", got)
	}
	env.clock.Advance(2 * time.Second)
	if got := levels(t, env, "a"); got["a"] != 3 { // lazily released by GetStock
		t.Fatalf("after expiry: %v", got)
	}
	if _, err := env.svc.Commit(ctx, &pb.CommitRequest{OrderId: "o1"}); code(err) != codes.FailedPrecondition {
		t.Fatalf("Commit after expiry: %v", err)
	}
	if _, err := reserve(env, "o1", "a", 3); code(err) != codes.FailedPrecondition {
		t.Fatalf("Reserve of an expired order id: %v", err)
	}
}

func TestExpiryLazyInReserveAndCommit(t *testing.T) {
	env := newTestService(t, map[string]int32{"a": 3})
	if _, err := reserve(env, "o1", "a", 3); err != nil {
		t.Fatal(err)
	}
	env.clock.Advance(16 * time.Minute)
	// another order can take the stock the expired hold had
	if r, err := reserve(env, "o2", "a", 3); err != nil || !r.Reserved {
		t.Fatalf("Reserve after expiry: %v %v", r, err)
	}
	// Commit of the expired one: refused, and the stock is not returned a second time
	if _, err := env.svc.Commit(ctx, &pb.CommitRequest{OrderId: "o1"}); code(err) != codes.FailedPrecondition {
		t.Fatalf("Commit: %v", err)
	}
	if got := env.store.available("a"); got != 0 {
		t.Fatalf("available = %d, want 0 (o2 holds it)", got)
	}
}

func TestSweep(t *testing.T) {
	env := newTestService(t, map[string]int32{"a": 10})
	for _, o := range []string{"o1", "o2", "o3"} {
		if _, err := reserve(env, o, "a", 2); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := env.svc.Commit(ctx, &pb.CommitRequest{OrderId: "o3"}); err != nil {
		t.Fatal(err)
	}
	if n, err := env.svc.SweepOnce(ctx); err != nil || n != 0 {
		t.Fatalf("early sweep: %d %v", n, err)
	}
	env.clock.Advance(time.Hour)
	if n, err := env.svc.SweepOnce(ctx); err != nil || n != 2 { // o3 is committed and never expires
		t.Fatalf("sweep: %d %v", n, err)
	}
	if got := env.store.available("a"); got != 8 {
		t.Fatalf("available = %d, want 8", got)
	}
	if n, _ := env.svc.SweepOnce(ctx); n != 0 {
		t.Fatalf("second sweep released %d", n)
	}
}

func TestSweeperRuns(t *testing.T) {
	clk := newClock()
	st := newMemStore(map[string]int32{"a": 3})
	logs := &bytes.Buffer{}
	svc := NewService(st, testLogger(logs), Config{ReservationTTL: time.Minute, SweepEvery: 5 * time.Millisecond, Now: clk.Now})
	svc.SetReady(true)
	if _, err := svc.Reserve(ctx, &pb.ReserveRequest{OrderId: "o1", Items: items("a", 3)}); err != nil {
		t.Fatal(err)
	}
	sctx, stop := context.WithCancel(ctx)
	svc.RunSweeper(sctx)
	time.Sleep(30 * time.Millisecond)
	if got := st.available("a"); got != 0 {
		t.Fatalf("swept before expiry: %d", got)
	}
	clk.Advance(2 * time.Minute)
	deadline := time.Now().Add(2 * time.Second)
	for st.available("a") != 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	stop()
	svc.Wait()
	if got := st.available("a"); got != 3 {
		t.Fatalf("available = %d, want 3 after the sweep", got)
	}
	if !strings.Contains(logs.String(), "sweep released 1 expired reservation(s)") {
		t.Fatalf("no sweep log line:\n%s", logs.String())
	}
}

func TestSweepIsOffUntilReady(t *testing.T) {
	env := newTestService(t, map[string]int32{"a": 1})
	env.svc.SetReady(false)
	env.store.setErr(errors.New("must not be called"))
	if n, err := env.svc.SweepOnce(ctx); n != 0 || err != nil {
		t.Fatalf("SweepOnce while not ready: %d %v", n, err)
	}
}

func TestUnknownProductIsMissingFromGetStock(t *testing.T) {
	env := newTestService(t, map[string]int32{"a": 4})
	got := levels(t, env, "ghost", "a", "ghost")
	if len(got) != 1 || got["a"] != 4 {
		t.Fatalf("levels = %v", got)
	}
}

func TestInvalidRequestsAreInvalidArgument(t *testing.T) {
	env := newTestService(t, map[string]int32{"a": 4})
	if _, err := env.svc.GetStock(ctx, &pb.GetStockRequest{}); code(err) != codes.InvalidArgument {
		t.Errorf("GetStock: %v", err)
	}
	if _, err := reserve(env, "o1"); code(err) != codes.InvalidArgument {
		t.Errorf("Reserve: %v", err)
	}
	if _, err := env.svc.Commit(ctx, &pb.CommitRequest{OrderId: "-bad"}); code(err) != codes.InvalidArgument {
		t.Errorf("Commit: %v", err)
	}
	if _, err := env.svc.Release(ctx, &pb.ReleaseRequest{}); code(err) != codes.InvalidArgument {
		t.Errorf("Release: %v", err)
	}
}

func TestNotReadyAndStoreErrors(t *testing.T) {
	env := newTestService(t, map[string]int32{"a": 4})
	env.svc.SetReady(false)
	if _, err := reserve(env, "o1", "a", 1); code(err) != codes.Unavailable {
		t.Errorf("not ready: %v", err)
	}
	env.svc.SetReady(true)
	secret := "pq: relation stock at 10.1.2.3 password=hunter2"
	env.store.setErr(errors.Join(ErrUnavailable, errors.New(secret)))
	_, err := env.svc.GetStock(ctx, &pb.GetStockRequest{ProductIds: []string{"a"}})
	if code(err) != codes.Unavailable || strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "10.1.2.3") {
		t.Errorf("unavailable: %v", err)
	}
	env.store.setErr(errors.New("syntax error at or near SELECT"))
	_, err = env.svc.Commit(ctx, &pb.CommitRequest{OrderId: "o1"})
	if code(err) != codes.Internal || strings.Contains(err.Error(), "syntax") {
		t.Errorf("internal: %v", err)
	}
	env.store.setErr(context.DeadlineExceeded)
	if _, err := env.svc.Release(ctx, &pb.ReleaseRequest{OrderId: "o1"}); code(err) != codes.DeadlineExceeded {
		t.Errorf("deadline: %v", err)
	}
}

func TestLogsOnlyOrderIDAndCounts(t *testing.T) {
	env := newTestService(t, map[string]int32{"secret-product-sku": 4})
	_, _ = reserve(env, "o1", "secret-product-sku", 1)
	_, _ = reserve(env, "o2", "secret-product-sku", 99)
	_, _ = env.svc.Commit(ctx, &pb.CommitRequest{OrderId: "o1"})
	_, _ = env.svc.Release(ctx, &pb.ReleaseRequest{OrderId: "o2"})
	out := env.logs.String()
	if !strings.Contains(out, "order=o1") || !strings.Contains(out, "shortages=1") {
		t.Fatalf("control failed: the log does not show what it should:\n%s", out)
	}
	if strings.Contains(out, "secret-product-sku") {
		t.Fatalf("product id in the log:\n%s", out)
	}
}

func TestConcurrentReservesNeverOversellInMemory(t *testing.T) {
	env := newTestService(t, map[string]int32{"a": 5})
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := reserve(env, "o"+string(rune('A'+i%26))+string(rune('a'+i/26)), "a", 1)
			if err == nil && r.Reserved {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if won != 5 || env.store.available("a") != 0 {
		t.Fatalf("won %d, available %d", won, env.store.available("a"))
	}
}
