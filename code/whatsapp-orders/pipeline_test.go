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

// fakeReader stands in for Claude. Without an order waiting for confirmation, anything
// mentioning "pesan" is an order of 2 dimsum. With one waiting, "ok" confirms it,
// "batal" cancels it and "jadi 3" changes it to 3.
type fakeReader struct {
	mu       sync.Mutex
	batches  [][]Incoming
	previous []*Order
	failing  int   // fail this many calls first
	failWith error // with this error; "API overloaded" when nil
}

func (f *fakeReader) Extract(ctx context.Context, batch []Incoming, previous *Order) (Reading, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches = append(f.batches, batch)
	f.previous = append(f.previous, previous)
	if f.failing > 0 {
		f.failing--
		if f.failWith != nil {
			return Reading{}, f.failWith
		}
		return Reading{}, errors.New("API overloaded")
	}
	text := batch[len(batch)-1].Text
	order := func(qty float64) Order {
		return Order{IsOrder: true, Items: []OrderItem{{Product: "Dimsum", Quantity: qty}}, Summary: "dimsum for " + batch[0].SenderName}
	}
	switch {
	case previous != nil && text == "ok":
		return Reading{AboutPrevious: "confirms", Model: "fake-model"}, nil
	case previous != nil && text == "batal":
		return Reading{AboutPrevious: "cancels", Model: "fake-model"}, nil
	case previous != nil && strings.Contains(text, "jadi 3"):
		return Reading{Order: order(3), AboutPrevious: "changes", Model: "fake-model"}, nil
	}
	for _, m := range batch {
		if strings.Contains(m.Text, "pesan") {
			return Reading{Order: order(2), AboutPrevious: "unrelated", Model: "fake-model"}, nil
		}
	}
	return Reading{AboutPrevious: "unrelated", Model: "fake-model"}, nil
}

// previousAt returns the order shown as waiting for confirmation in call i.
func (f *fakeReader) previousAt(i int) *Order {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.previous[i]
}

func (f *fakeReader) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.batches)
}

func testPipeline(t *testing.T, reader orderReader, edits ...func(*Config)) (*Pipeline, *Store, string) {
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
	for _, edit := range edits {
		edit(&cfg)
	}
	store := openTestStore(t)
	logPath := filepath.Join(t.TempDir(), "orders.jsonl")
	p := NewPipeline(context.Background(), cfg, store, reader,
		NewDeliverer(OutputConfig{}, store, logPath), NewReplier(cfg.Reply, store), false)
	p.retryIn = 10 * time.Millisecond
	return p, store, logPath
}

// withReplies turns confirmation messages on, sent through outbox.
func withReplies(t *testing.T, p *Pipeline, outbox *sentMessages) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go p.replier.Run(ctx, outbox.send)
}

func sendMode(c *Config) {
	c.Reply = ReplyConfig{
		Mode:          "send",
		Confirm:       "Halo {name}:\n{items}\nBalas YA",
		Thanks:        "Makasih {name}",
		MinGap:        time.Millisecond,
		MaxPerHour:    100,
		ConfirmWindow: time.Hour,
	}
}

func readEvents(t *testing.T, path string) []OrderEvent {
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

func messageStatus(s *Store, id string) string {
	var status string
	s.db.QueryRow(`SELECT status FROM messages WHERE id = ?`, id).Scan(&status)
	return status
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
	waitFor(t, "two orders", func() bool { return len(readEvents(t, logPath)) == 2 })
	time.Sleep(50 * time.Millisecond)
	if reader.calls() != 3 {
		t.Fatalf("Claude was asked %d times, want 3 (Budi, Dewi, Eka)", reader.calls())
	}

	byName := map[string]OrderEvent{}
	for _, ev := range readEvents(t, logPath) {
		byName[ev.Source.SenderName] = ev
	}
	budi, dewi := byName["Budi"], byName["Dewi"]
	if budi.Type != "order.created" || budi.OrderID != budi.ID || budi.Order == nil {
		t.Errorf("Budi's event = %+v", budi)
	}
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
	waitFor(t, "the order once Claude is back", func() bool { return len(readEvents(t, logPath)) == 1 })
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
	waitFor(t, "the resumed order", func() bool { return len(readEvents(t, logPath)) == 1 })
	waitFor(t, "status ordered", func() bool { return messageStatus(store, "1") == "ordered" })
}

func budiSays(id, text string) Incoming {
	return Incoming{ID: id, ChatID: "6281111111111@s.whatsapp.net", SenderID: "6281111111111@s.whatsapp.net",
		SenderPhone: "6281111111111", SenderName: "Budi", Time: time.Now(), Text: text}
}

func TestPipelineCustomerConfirms(t *testing.T) {
	reader := &fakeReader{}
	p, store, logPath := testPipeline(t, reader, sendMode)
	var outbox sentMessages
	withReplies(t, p, &outbox)

	p.Handle(budiSays("1", "pesan 2 dimsum"))
	waitFor(t, "the confirmation request", func() bool { return outbox.count() == 1 })
	if got := outbox.get(0); got.chatID != "6281111111111@s.whatsapp.net" || got.text != "Halo Budi:\n- 2 Dimsum\nBalas YA" {
		t.Errorf("confirmation request = %+v", got)
	}
	waitFor(t, "the request to be recorded", func() bool { return p.waitingOrder(budiSays("", "")) != nil })

	// "ok" is in filter.skip_phrases, but here it answers the confirmation request.
	p.Handle(budiSays("2", "ok"))
	waitFor(t, "the confirmed event", func() bool { return len(readEvents(t, logPath)) == 2 })
	events := readEvents(t, logPath)
	created, confirmed := events[0], events[1]
	if confirmed.Type != "order.confirmed" || confirmed.OrderID != created.OrderID || confirmed.Order != nil ||
		len(confirmed.Messages) != 1 || confirmed.Messages[0].Text != "ok" {
		t.Errorf("confirmed event = %+v", confirmed)
	}
	if prev := reader.previousAt(1); prev == nil || prev.Items[0].Quantity != 2 {
		t.Error("Claude was not shown the order waiting for confirmation")
	}
	waitFor(t, "the thank-you message", func() bool { return outbox.count() == 2 })
	if got := outbox.get(1).text; got != "Makasih Budi" {
		t.Errorf("thank-you message = %q", got)
	}
	if p.waitingOrder(budiSays("", "")) != nil {
		t.Error("the order is still waiting after it was confirmed")
	}
	if messageStatus(store, "2") != "confirmed" {
		t.Errorf("status of the answer = %q", messageStatus(store, "2"))
	}
}

func TestPipelineCustomerChangesThenCancels(t *testing.T) {
	reader := &fakeReader{}
	p, _, logPath := testPipeline(t, reader, sendMode)
	var outbox sentMessages
	withReplies(t, p, &outbox)

	p.Handle(budiSays("1", "pesan 2 dimsum"))
	waitFor(t, "the first request to be recorded", func() bool { return p.waitingOrder(budiSays("", "")) != nil })

	p.Handle(budiSays("2", "jadi 3 ya"))
	waitFor(t, "the corrected order", func() bool { return len(readEvents(t, logPath)) == 2 })
	events := readEvents(t, logPath)
	if events[1].Type != "order.created" || events[1].Replaces != events[0].OrderID || events[1].Order.Items[0].Quantity != 3 {
		t.Errorf("corrected order = %+v", events[1])
	}
	waitFor(t, "a confirmation request for the corrected order", func() bool {
		w := p.waitingOrder(budiSays("", ""))
		return w != nil && w.ID == events[1].OrderID
	})

	p.Handle(budiSays("3", "batal"))
	waitFor(t, "the cancellation", func() bool { return len(readEvents(t, logPath)) == 3 })
	if ev := readEvents(t, logPath)[2]; ev.Type != "order.cancelled" || ev.OrderID != events[1].OrderID {
		t.Errorf("cancellation = %+v", ev)
	}
	if p.waitingOrder(budiSays("", "")) != nil {
		t.Error("the order is still waiting after it was cancelled")
	}
	if outbox.count() != 2 {
		t.Errorf("sent %d messages, want 2 confirmation requests and no thank-you", outbox.count())
	}
}

func TestPipelinePreviewSendsNothing(t *testing.T) {
	reader := &fakeReader{}
	p, _, logPath := testPipeline(t, reader, sendMode, func(c *Config) { c.Reply.Mode = "preview" })
	var outbox sentMessages
	withReplies(t, p, &outbox)

	p.Handle(budiSays("1", "pesan 2 dimsum"))
	waitFor(t, "the order", func() bool { return len(readEvents(t, logPath)) == 1 })
	time.Sleep(50 * time.Millisecond)
	if outbox.count() != 0 || p.waitingOrder(budiSays("", "")) != nil {
		t.Error("preview mode sent a message")
	}
}
