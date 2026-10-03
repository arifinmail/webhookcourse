package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeReader stands in for Claude: anything mentioning "pesan" is an order.
type fakeReader struct {
	mu       sync.Mutex
	batches  [][]Incoming
	failing  int   // fail this many calls first
	failWith error // with this error; "API overloaded" when nil
}

func (f *fakeReader) Extract(ctx context.Context, batch []Incoming) (Order, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches = append(f.batches, batch)
	if f.failing > 0 {
		f.failing--
		if f.failWith != nil {
			return Order{}, "", f.failWith
		}
		return Order{}, "", errors.New("API overloaded")
	}
	for _, m := range batch {
		if strings.Contains(m.Text, "pesan") {
			return Order{IsOrder: true, Summary: "order from " + m.SenderName}, "fake-model", nil
		}
	}
	return Order{}, "fake-model", nil
}

func (f *fakeReader) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.batches)
}

func testPipeline(t *testing.T, reader orderReader) (*Pipeline, *Store, string) {
	t.Helper()
	cfg := defaultConfig()
	cfg.Filter = FilterConfig{
		PrivateChats:  true,
		Groups:        []string{"PO Oktober"},
		IgnoreNumbers: []string{"0812-0000-0000"},
		SkipPhrases:   []string{"ok"},
		MinLength:     2,
		MaxAge:        time.Hour,
	}
	cfg.Batching = BatchConfig{QuietPeriod: 30 * time.Millisecond, MaxWait: time.Second}
	store := openTestStore(t)
	logPath := filepath.Join(t.TempDir(), "orders.jsonl")
	p := NewPipeline(context.Background(), cfg, store, reader, NewDeliverer(OutputConfig{}, store, logPath), false)
	p.retryIn = 10 * time.Millisecond
	return p, store, logPath
}

func readOrders(t *testing.T, path string) []OrderEvent {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []OrderEvent
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var ev OrderEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
	return out
}

func TestPipelineEndToEnd(t *testing.T) {
	reader := &fakeReader{}
	p, store, logPath := testPipeline(t, reader)
	now := time.Now()
	private := func(id, phone, name, text string, at time.Time) Incoming {
		return Incoming{ID: id, ChatID: phone + "@s.whatsapp.net", SenderID: phone + "@s.whatsapp.net",
			SenderPhone: phone, SenderName: name, Time: at, Text: text}
	}
	inGroup := func(id, group, phone, name, text string) Incoming {
		return Incoming{ID: id, ChatID: group + "@g.us", ChatName: group, IsGroup: true,
			SenderID: phone + "@s.whatsapp.net", SenderPhone: phone, SenderName: name, Time: now, Text: text}
	}

	p.Handle(private("a2", "6281111111111", "Budi", "2 dimsum ayam", now.Add(time.Second)))
	p.Handle(private("a1", "6281111111111", "Budi", "Kak mau pesan", now)) // arrives late, read first
	p.Handle(private("a1", "6281111111111", "Budi", "Kak mau pesan", now)) // WhatsApp delivered it twice
	p.Handle(private("f1", "6281200000000", "Mama", "pesan martabak ya", now))
	p.Handle(private("c1", "6283333333333", "Cici", "ok", now))
	p.Handle(inGroup("g1", "Keluarga", "6284444444444", "Om", "pesan apa?"))
	p.Handle(inGroup("g2", "PO Oktober", "6285555555555", "Dewi", "pesan 1 risoles"))
	p.Handle(private("q1", "6286666666666", "Eka", "jam berapa buka?", now))

	waitFor(t, "three batches", func() bool { return reader.calls() == 3 })
	waitFor(t, "two orders", func() bool { return len(readOrders(t, logPath)) == 2 })
	time.Sleep(50 * time.Millisecond)
	if reader.calls() != 3 {
		t.Fatalf("Claude was asked %d times, want 3 (Budi, Dewi, Eka)", reader.calls())
	}

	orders := readOrders(t, logPath)
	byName := map[string]OrderEvent{}
	for _, ev := range orders {
		byName[ev.Source.SenderName] = ev
	}
	budi, dewi := byName["Budi"], byName["Dewi"]
	if len(budi.Messages) != 2 || budi.Messages[0].Text != "Kak mau pesan" || budi.Source.ChatType != "private" {
		t.Errorf("Budi's order = %+v", budi)
	}
	if dewi.Source.ChatType != "group" || dewi.Source.ChatName != "PO Oktober" || dewi.Model != "fake-model" {
		t.Errorf("Dewi's order = %+v", dewi)
	}

	pending, err := store.PendingMessages(context.Background())
	if err != nil || len(pending) != 0 {
		t.Errorf("pending messages left: %v %v", pending, err)
	}
}

func TestPipelineKeepsRetryingWhenClaudeIsUnreachable(t *testing.T) {
	reader := &fakeReader{failing: maxReadAttempts + 2}
	p, store, logPath := testPipeline(t, reader)
	p.Handle(Incoming{ID: "1", ChatID: "x@s.whatsapp.net", SenderID: "x", SenderName: "Budi", Time: time.Now(), Text: "pesan 2"})
	waitFor(t, "the order once Claude is back", func() bool { return len(readOrders(t, logPath)) == 1 })
	waitFor(t, "status ordered", func() bool { return messageStatus(store, "1") == "ordered" })
}

func TestPipelineGivesUpOnBadAnswers(t *testing.T) {
	reader := &fakeReader{failing: maxReadAttempts, failWith: badAnswer{errors.New("not an order")}}
	p, store, _ := testPipeline(t, reader)
	p.Handle(Incoming{ID: "1", ChatID: "y@s.whatsapp.net", SenderID: "y", SenderName: "Dewi", Time: time.Now(), Text: "pesan 3"})
	waitFor(t, "status error", func() bool { return messageStatus(store, "1") == "error" })
	if reader.calls() != maxReadAttempts {
		t.Errorf("Claude was asked %d times, want %d", reader.calls(), maxReadAttempts)
	}
}

func TestPipelineResumesPendingMessages(t *testing.T) {
	reader := &fakeReader{}
	p, store, logPath := testPipeline(t, reader)
	// A message saved just before the program stopped, never read.
	if _, err := store.SaveMessage(context.Background(), Incoming{ID: "1", ChatID: "x@s.whatsapp.net", SenderID: "x",
		SenderName: "Budi", Time: time.Now(), Text: "pesan 2"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Resume(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the resumed order", func() bool { return len(readOrders(t, logPath)) == 1 })
	waitFor(t, "status ordered", func() bool { return messageStatus(store, "1") == "ordered" })
}

func messageStatus(s *Store, id string) string {
	var status string
	s.db.QueryRow(`SELECT status FROM messages WHERE id = ?`, id).Scan(&status)
	return status
}
