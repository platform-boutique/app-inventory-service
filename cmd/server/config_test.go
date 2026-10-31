package main

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func clearTelemetryEnv(t *testing.T) {
	for _, k := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "COLLECTOR_SERVICE_ADDR", "OTEL_SDK_DISABLED", "ENABLE_TRACING", "PYROSCOPE_ADDR", "ENABLE_PROFILING"} {
		t.Setenv(k, "")
	}
}

func TestTelemetryOffByDefault(t *testing.T) {
	clearTelemetryEnv(t)
	if ep := otlpEndpoint(); ep != "" {
		t.Fatalf("endpoint %q with everything unset", ep)
	}
	shutdown, err := initTelemetry(context.Background(), serviceName, log)
	if !errors.Is(err, errTelemetryOff) {
		t.Fatalf("initTelemetry err = %v, want errTelemetryOff", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("no-op shutdown: %v", err)
	}
	if _, on := profilingConfig(); on {
		t.Fatal("profiling on with everything unset")
	}
}

func TestTelemetrySwitches(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"endpoint", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://alloy:4317"}, "http://alloy:4317"},
		{"blank endpoint", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "  "}, ""},
		{"legacy addr", map[string]string{"COLLECTOR_SERVICE_ADDR": "alloy:4317"}, "http://alloy:4317"},
		{"sdk disabled", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://alloy:4317", "OTEL_SDK_DISABLED": "true"}, ""},
		{"sdk disabled upper", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://alloy:4317", "OTEL_SDK_DISABLED": "TRUE"}, ""},
		{"sdk disabled false", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://alloy:4317", "OTEL_SDK_DISABLED": "false"}, "http://alloy:4317"},
		{"enable_tracing 0", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://alloy:4317", "ENABLE_TRACING": "0"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearTelemetryEnv(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			if got := otlpEndpoint(); got != c.want {
				t.Errorf("otlpEndpoint() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestProfilingSwitch(t *testing.T) {
	cases := []struct {
		addr, enable string
		on           bool
	}{
		{"", "", false},
		{"", "1", false},
		{"http://alloy:4040", "", true},
		{"http://alloy:4040", "1", true},
		{"http://alloy:4040", "0", false},
	}
	for _, c := range cases {
		clearTelemetryEnv(t)
		t.Setenv("PYROSCOPE_ADDR", c.addr)
		t.Setenv("ENABLE_PROFILING", c.enable)
		if addr, on := profilingConfig(); on != c.on || (on && addr != c.addr) {
			t.Errorf("PYROSCOPE_ADDR=%q ENABLE_PROFILING=%q: on=%v addr=%q, want on=%v", c.addr, c.enable, on, addr, c.on)
		}
	}
}

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
