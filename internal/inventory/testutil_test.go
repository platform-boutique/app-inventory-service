package inventory

import (
	"bytes"
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

// fakeClock is a settable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 31, 12, 0, 0, 123_456_789, time.UTC)}
}
func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type memRes struct {
	status    string
	items     []Item
	expiresAt time.Time
}

// memStore is an in-memory Store with the same semantics as PGStore (one mutex instead of row locks).
type memStore struct {
	mu    sync.Mutex
	stock map[string]int32
	res   map[string]*memRes
	err   error // returned by every call when set
}

func newMemStore(stock map[string]int32) *memStore {
	return &memStore{stock: stock, res: map[string]*memRes{}}
}

func (m *memStore) release(r *memRes) {
	for _, it := range r.items {
		m.stock[it.ProductID] += it.Quantity
	}
	r.status = StatusReleased
}

func (m *memStore) expire(now time.Time, only map[string]bool) int {
	n := 0
	for _, r := range m.res {
		if r.status != StatusReserved || r.expiresAt.After(now) {
			continue
		}
		touches := only == nil
		for _, it := range r.items {
			if only[it.ProductID] {
				touches = true
			}
		}
		if touches {
			m.release(r)
			n++
		}
	}
	return n
}

func (m *memStore) GetStock(_ context.Context, ids []string, now time.Time) ([]StockLevel, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	m.expire(now, set)
	var out []StockLevel
	for _, id := range ids {
		if n, ok := m.stock[id]; ok {
			out = append(out, StockLevel{ProductID: id, Available: n})
		}
	}
	return out, nil
}

func (m *memStore) Reserve(_ context.Context, orderID string, items []Item, now time.Time, ttl time.Duration) (ReserveResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return ReserveResult{}, m.err
	}
	set := map[string]bool{}
	for _, it := range items {
		set[it.ProductID] = true
	}
	delete(set, "") // keep the order's own hold for the explicit handling below
	if r, ok := m.res[orderID]; ok {
		switch {
		case r.status == StatusReleased:
			return ReserveResult{}, ErrReleased
		case r.status == StatusReserved && !r.expiresAt.After(now):
			m.release(r)
			return ReserveResult{}, ErrReleased
		case !SameItems(r.items, items):
			return ReserveResult{}, ErrItemsDiffer
		}
		return ReserveResult{Reserved: true}, nil
	}
	m.expire(now, set)
	var short []Shortage
	for _, it := range items {
		if a := m.stock[it.ProductID]; a < it.Quantity {
			short = append(short, Shortage{ProductID: it.ProductID, Requested: it.Quantity, Available: a})
		}
	}
	if len(short) > 0 {
		return ReserveResult{Shortages: short}, nil
	}
	for _, it := range items {
		m.stock[it.ProductID] -= it.Quantity
	}
	m.res[orderID] = &memRes{status: StatusReserved, items: append([]Item(nil), items...), expiresAt: now.Add(ttl)}
	return ReserveResult{Reserved: true}, nil
}

func (m *memStore) Commit(_ context.Context, orderID string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	r, ok := m.res[orderID]
	switch {
	case !ok:
		return ErrNotFound
	case r.status == StatusCommitted:
		return nil
	case r.status == StatusReleased:
		return ErrReleased
	case !r.expiresAt.After(now):
		m.release(r)
		return ErrReleased
	}
	r.status = StatusCommitted
	return nil
}

func (m *memStore) Release(_ context.Context, orderID string, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	r, ok := m.res[orderID]
	switch {
	case !ok, ok && r.status == StatusReleased:
		return nil
	case r.status == StatusCommitted:
		return ErrCommitted
	}
	m.release(r)
	return nil
}

func (m *memStore) Sweep(_ context.Context, now time.Time, limit int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return 0, m.err
	}
	var ids []string
	for id, r := range m.res {
		if r.status == StatusReserved && !r.expiresAt.After(now) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) > limit {
		ids = ids[:limit]
	}
	for _, id := range ids {
		m.release(m.res[id])
	}
	return len(ids), nil
}

func (m *memStore) Ping(context.Context) error { return m.err }

func (m *memStore) setErr(err error) { m.mu.Lock(); m.err = err; m.mu.Unlock() }

func (m *memStore) available(id string) int32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stock[id]
}

func testLogger(w *bytes.Buffer) *logrus.Logger {
	l := logrus.New()
	l.SetLevel(logrus.DebugLevel)
	l.Formatter = &logrus.JSONFormatter{}
	l.Out = w
	return l
}

type testEnv struct {
	svc   *Service
	store *memStore
	clock *fakeClock
	logs  *bytes.Buffer
}

func newTestService(t *testing.T, stock map[string]int32) *testEnv {
	t.Helper()
	clk := newClock()
	st := newMemStore(stock)
	logs := &bytes.Buffer{}
	svc := NewService(st, testLogger(logs), Config{ReservationTTL: 15 * time.Minute, Now: clk.Now})
	svc.SetReady(true)
	t.Cleanup(svc.Wait)
	return &testEnv{svc: svc, store: st, clock: clk, logs: logs}
}
