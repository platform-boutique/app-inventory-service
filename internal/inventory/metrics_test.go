package inventory

import (
	"context"
	"errors"
	"fmt"
	"testing"

	dto "github.com/prometheus/client_model/go"
)

func TestReserveOutcomeMapping(t *testing.T) {
	cases := []struct {
		res  ReserveResult
		err  error
		want string
	}{
		{ReserveResult{Reserved: true}, nil, "reserved"},
		{ReserveResult{Reserved: false}, nil, "shortage"},
		{ReserveResult{}, ErrItemsDiffer, "conflict"},
		{ReserveResult{}, fmt.Errorf("wrapped: %w", ErrItemsDiffer), "conflict"},
		{ReserveResult{}, ErrInvalid, "rejected"},
		{ReserveResult{}, ErrReleased, "rejected"},
		{ReserveResult{}, ErrCommitted, "rejected"},
		{ReserveResult{}, ErrUnavailable, "unavailable"},
		{ReserveResult{}, context.Canceled, "canceled"},
		{ReserveResult{}, context.DeadlineExceeded, "canceled"},
		{ReserveResult{}, errors.New("boom"), "error"},
	}
	for _, c := range cases {
		if got := reserveOutcome(c.res, c.err); got != c.want {
			t.Errorf("reserveOutcome(%+v, %v) = %q, want %q", c.res, c.err, got, c.want)
		}
	}
}

func TestEveryOutcomeIsAKnownLabelValue(t *testing.T) {
	known := map[string]bool{}
	for _, o := range reservationOutcomes {
		known[o] = true
		// the series exist from the start (value readable without having been incremented by a test)
		var m dto.Metric
		if err := reservationsTotal.WithLabelValues(o).Write(&m); err != nil || m.GetCounter() == nil {
			t.Errorf("series %s not readable: %v", o, err)
		}
	}
	for _, e := range []error{nil, ErrItemsDiffer, ErrInvalid, ErrReleased, ErrCommitted, ErrNotFound, ErrUnavailable, context.Canceled, errors.New("x")} {
		for _, reserved := range []bool{true, false} {
			if o := reserveOutcome(ReserveResult{Reserved: reserved}, e); !known[o] {
				t.Errorf("outcome %q is not in reservationOutcomes", o)
			}
		}
	}
}
