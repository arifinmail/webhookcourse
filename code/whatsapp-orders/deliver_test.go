package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "orders.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestDelivererSignsAndRetries(t *testing.T) {
	const secret = "s3cret"
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if got, want := r.Header.Get("X-Signature-256"), "sha256="+sign(secret, body); got != want {
			t.Errorf("signature = %q, want %q", got, want)
		}
		if r.Header.Get("X-Event-Id") != "ord_1" {
			t.Errorf("X-Event-Id = %q", r.Header.Get("X-Event-Id"))
		}
		if calls.Add(1) == 1 {
			http.Error(w, "down for maintenance", http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()

	store := openTestStore(t)
	logPath := filepath.Join(t.TempDir(), "orders.jsonl")
	d := NewDeliverer(OutputConfig{WebhookURL: srv.URL, Secret: secret}, store, logPath)
	ctx := context.Background()
	if err := d.Enqueue(ctx, OrderEvent{ID: "ord_1", Type: "order.created", OrderID: "ord_1", Order: &Order{IsOrder: true, Summary: "2 dimsum"}}); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	d.deliverDue(ctx, now) // 503: kept for a retry a minute later
	d.deliverDue(ctx, now) // not due yet
	if calls.Load() != 1 {
		t.Fatalf("calls = %d after the failure, want 1", calls.Load())
	}
	d.deliverDue(ctx, now.Add(2*time.Minute)) // retry succeeds
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
	if due, _ := store.DueEvents(ctx, now.Add(24*time.Hour)); len(due) != 0 {
		t.Errorf("order still waiting after it was delivered")
	}
	logged, _ := os.ReadFile(logPath)
	if strings.Count(string(logged), "\n") != 1 || !strings.Contains(string(logged), `"summary":"2 dimsum"`) {
		t.Errorf("orders.jsonl = %q", logged)
	}
}

func TestDelivererTestModeOnlyLogs(t *testing.T) {
	store := openTestStore(t)
	logPath := filepath.Join(t.TempDir(), "orders.jsonl")
	d := NewDeliverer(OutputConfig{}, store, logPath)
	ctx := context.Background()
	if err := d.Enqueue(ctx, OrderEvent{ID: "ord_1"}); err != nil {
		t.Fatal(err)
	}
	if due, _ := store.DueEvents(ctx, time.Now()); len(due) != 0 {
		t.Error("test mode should not queue orders for sending")
	}
	if logged, _ := os.ReadFile(logPath); !strings.Contains(string(logged), `"id":"ord_1"`) {
		t.Errorf("orders.jsonl = %q", logged)
	}
}
