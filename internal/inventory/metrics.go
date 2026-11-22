package inventory

import (
	"context"
	"errors"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Business counter (OBS-20: stock reservations by outcome), next to the RED series of the gRPC middleware and
// the db_pool_* series. The label is a fixed small set, never an order id, product id or message.
var reservationsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "inventory_reservations_total",
	Help: "Reserve calls by outcome: reserved, shortage, conflict (same order id with other items), rejected (invalid request or reservation already released/committed), unavailable (database), canceled (client gave up or deadline), error.",
}, []string{"outcome"})

var reservationOutcomes = []string{"reserved", "shortage", "conflict", "rejected", "unavailable", "canceled", "error"}

func init() {
	// The series exist (at 0) from the start, so a rate() over them works before the first reservation.
	for _, o := range reservationOutcomes {
		reservationsTotal.WithLabelValues(o)
	}
}

// reserveOutcome maps the result of a Reserve call to the label value of inventory_reservations_total.
func reserveOutcome(res ReserveResult, err error) string {
	switch {
	case err == nil && res.Reserved:
		return "reserved"
	case err == nil:
		return "shortage"
	case errors.Is(err, ErrItemsDiffer):
		return "conflict"
	case errors.Is(err, ErrInvalid), errors.Is(err, ErrReleased), errors.Is(err, ErrCommitted), errors.Is(err, ErrNotFound):
		return "rejected"
	case errors.Is(err, ErrUnavailable):
		return "unavailable"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	default:
		return "error"
	}
}
