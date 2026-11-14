package main

import (
	"io"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func cfgEnv(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func quietLog() *logrus.Logger {
	l := logrus.New()
	l.Out = io.Discard
	return l
}

func TestConfigDefaults(t *testing.T) {
	cfg, err := configFromEnv(cfgEnv(nil), quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReservationTTL != 15*time.Minute || cfg.SweepEvery != time.Minute {
		t.Errorf("ttl %s, sweep %s", cfg.ReservationTTL, cfg.SweepEvery)
	}
}

func TestConfigFromEnv(t *testing.T) {
	cfg, err := configFromEnv(cfgEnv(map[string]string{"RESERVATION_TTL": " 90s ", "SWEEP_INTERVAL": "5s"}), quietLog())
	if err != nil || cfg.ReservationTTL != 90*time.Second || cfg.SweepEvery != 5*time.Second {
		t.Fatalf("%+v, %v", cfg, err)
	}
}

func TestConfigBadDurationsAreErrors(t *testing.T) {
	for _, m := range []map[string]string{
		{"RESERVATION_TTL": "soon"}, {"RESERVATION_TTL": "0s"}, {"RESERVATION_TTL": "-1m"}, {"RESERVATION_TTL": "15"},
		{"SWEEP_INTERVAL": "often"}, {"SWEEP_INTERVAL": "0"}, {"SWEEP_INTERVAL": "-1s"},
	} {
		if _, err := configFromEnv(cfgEnv(m), quietLog()); err == nil {
			t.Errorf("%v accepted", m)
		}
	}
}
