package main

import (
	"fmt"
	"strings"
	"time"

	"inventory-service/internal/inventory"

	"github.com/sirupsen/logrus"
)

// configFromEnv reads the service settings:
//
//	RESERVATION_TTL  Go duration, how long a RESERVED hold lives before it is released (default 15m)
//	SWEEP_INTERVAL   Go duration, how often the background sweep looks for expired holds (default 1m)
//
// A bad or non-positive value is an error (the process exits): a typo must not silently change how long
// stock is held.
func configFromEnv(getenv func(string) string, log *logrus.Logger) (inventory.Config, error) {
	cfg := inventory.Config{ReservationTTL: inventory.DefaultReservationTTL, SweepEvery: inventory.DefaultSweepEvery}
	for _, s := range []struct {
		name string
		dst  *time.Duration
	}{{"RESERVATION_TTL", &cfg.ReservationTTL}, {"SWEEP_INTERVAL", &cfg.SweepEvery}} {
		v := strings.TrimSpace(getenv(s.name))
		if v == "" {
			continue
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return cfg, fmt.Errorf("%s=%q: want a positive Go duration such as 15m", s.name, v)
		}
		*s.dst = d
	}
	log.Infof("reservation ttl %s, sweep every %s", cfg.ReservationTTL, cfg.SweepEvery)
	return cfg, nil
}
