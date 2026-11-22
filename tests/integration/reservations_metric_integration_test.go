//go:build integration

package integration

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// reservationCount reads inventory_reservations_total{outcome} from the default registry (what /metrics serves).
func reservationCount(t *testing.T, outcome string) float64 {
	t.Helper()
	fams, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		if f.GetName() != "inventory_reservations_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "outcome" && l.GetValue() == outcome {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	t.Fatalf("inventory_reservations_total{outcome=%q} not found", outcome)
	return 0
}

// OBS-20: stock reservations by outcome, through the real service and a real database.
func TestReservationsAreCountedByOutcome(t *testing.T) {
	e := setup(t, options{})
	e.setStock(t, "a", 10)
	before := map[string]float64{}
	for _, o := range []string{"reserved", "shortage", "conflict", "rejected"} {
		before[o] = reservationCount(t, o)
	}
	if r, err := e.reserve("c1", "a", 3); err != nil || !r.Reserved { // reserved
		t.Fatal(r, err)
	}
	if r, err := e.reserve("c2", "a", 99); err != nil || r.Reserved { // shortage
		t.Fatal(r, err)
	}
	if _, err := e.reserve("c1", "a", 4); err == nil { // same order id, other items: conflict
		t.Fatal("want ALREADY_EXISTS")
	}
	if _, err := e.reserve("", "a", 1); err == nil { // invalid order id: rejected
		t.Fatal("want INVALID_ARGUMENT")
	}
	if r, err := e.reserve("c1", "a", 3); err != nil || !r.Reserved { // idempotent replay is a successful Reserve too
		t.Fatal(r, err)
	}
	want := map[string]float64{"reserved": 2, "shortage": 1, "conflict": 1, "rejected": 1}
	for o, w := range want {
		if got := reservationCount(t, o) - before[o]; got != w {
			t.Errorf("outcome %s: +%v, want +%v", o, got, w)
		}
	}
}
