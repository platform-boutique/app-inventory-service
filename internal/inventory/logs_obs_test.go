package inventory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"inventory-service/internal/obs"
	pb "inventory-service/proto"

	"go.opentelemetry.io/otel/trace"
)

func spanCtx() context.Context {
	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	c := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled}))
	return obs.WithRequestID(c, "req-5")
}

func jsonLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not JSON: %q", l)
		}
		out = append(out, m)
	}
	return out
}

// OBS-30: the request path logs in the contract's shape with the request's trace ids; an
// internal error shows up in the error field and its detail never reaches the caller.
func TestRequestPathLogsCarryTraceIDs(t *testing.T) {
	env := newTestService(t, map[string]int32{"a": 10})
	env.svc.log.SetFormatter(obs.NewFormatter("inventory-service", "test"))

	c := spanCtx()
	if _, err := env.svc.Reserve(c, &pb.ReserveRequest{OrderId: "o1", Items: items("a", 2)}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.Commit(c, &pb.CommitRequest{OrderId: "o1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.Commit(c, &pb.CommitRequest{OrderId: "o-none"}); err == nil {
		t.Fatal("expected NOT_FOUND")
	}
	env.store.setErr(errors.New("secret table detail"))
	if _, err := env.svc.GetStock(c, &pb.GetStockRequest{ProductIds: []string{"a"}}); code(err).String() != "Internal" {
		t.Fatalf("expected Internal, got %v", err)
	}

	lines := jsonLines(t, env.logs)
	if len(lines) < 3 {
		t.Fatalf("expected request logs, got:\n%s", env.logs.String())
	}
	sawError := false
	for _, m := range lines {
		if m["trace_id"] != "4bf92f3577b34da6a3ce929d0e0e4736" || m["span_id"] != "00f067aa0ba902b7" || m["request_id"] != "req-5" {
			t.Errorf("line without the request's ids: %v", m)
		}
		if m["service"] != "inventory-service" || m["severity"] == nil || m["timestamp"] == nil || m["message"] == nil {
			t.Errorf("shape: %v", m)
		}
		if m["severity"] == "error" {
			sawError = true
			if e, _ := m["error"].(string); !strings.Contains(e, "secret table detail") {
				t.Errorf("error line without the error field: %v", m)
			}
		}
	}
	if !sawError {
		t.Errorf("internal error not logged at error level:\n%s", env.logs.String())
	}
	if m := lines[0]; m["order_id"] != "o1" || m["message"] != "reserve" {
		t.Errorf("reserve line: %v", m)
	}
}

// Outside a span the ids are absent.
func TestLogsWithoutSpanHaveNoIDs(t *testing.T) {
	env := newTestService(t, map[string]int32{"a": 10})
	env.svc.log.SetFormatter(obs.NewFormatter("inventory-service", "test"))
	if _, err := env.svc.Reserve(context.Background(), &pb.ReserveRequest{OrderId: "o1", Items: items("a", 1)}); err != nil {
		t.Fatal(err)
	}
	for _, m := range jsonLines(t, env.logs) {
		for _, k := range []string{"trace_id", "span_id", "request_id"} {
			if _, ok := m[k]; ok {
				t.Errorf("%s present outside a span: %v", k, m)
			}
		}
	}
}
