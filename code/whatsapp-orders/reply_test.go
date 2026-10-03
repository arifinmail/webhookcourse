package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type sentMessage struct {
	chatID, text string
	at           time.Time
}

// sentMessages stands in for WhatsApp and keeps what was sent.
type sentMessages struct {
	mu        sync.Mutex
	list      []sentMessage
	calls     int
	failFirst int // fail this many sends first
}

func (s *sentMessages) send(ctx context.Context, chatID, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.failFirst > 0 {
		s.failFirst--
		return errors.New("not connected")
	}
	s.list = append(s.list, sentMessage{chatID: chatID, text: text, at: time.Now()})
	return nil
}

func (s *sentMessages) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.list)
}

func (s *sentMessages) get(i int) sentMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.list[i]
}

func TestConfirmText(t *testing.T) {
	cfg := ReplyConfig{
		Confirm: "Halo {name}, pesanan kamu:\n{items}\n\n{missing}\n\nKirim ke: {address}\nBalas YA kalau sudah benar.",
		Missing: "Mohon info juga: {list}",
	}
	order := Order{
		Items: []OrderItem{
			{Product: "Dimsum ayam (isi 10)", Quantity: 2, Unit: "pack"},
			{Product: "Sambal bawang", Quantity: 1.5, Notes: "pedas"},
			{Product: "Risoles"},
		},
		MissingInfo: []string{"alamat", "metode pembayaran"},
	}
	want := "Halo Budi, pesanan kamu:\n" +
		"- 2 pack Dimsum ayam (isi 10)\n- 1.5 Sambal bawang (pedas)\n- Risoles\n\n" +
		"Mohon info juga: alamat, metode pembayaran\n\n" +
		"Kirim ke:\nBalas YA kalau sudah benar."
	if got := confirmText(cfg, order, "Budi"); got != want {
		t.Errorf("got:\n%s\n\nwant:\n%s", got, want)
	}

	// The name the customer gave beats their WhatsApp name; nothing missing, no line.
	order.CustomerName, order.MissingInfo = "Pak Budi", nil
	got := confirmText(ReplyConfig{Confirm: "Halo {name} ,\n{items}\n{missing}\nOK?"}, order, "budi123")
	if want := "Halo Pak Budi,\n- 2 pack Dimsum ayam (isi 10)\n- 1.5 Sambal bawang (pedas)\n- Risoles\n\nOK?"; got != want {
		t.Errorf("got:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestReplierOnlyRepliesToPrivateOrders(t *testing.T) {
	order := Order{IsOrder: true, Items: []OrderItem{{Product: "Dimsum", Quantity: 1}}}
	private := Incoming{ChatID: "62811@s.whatsapp.net", SenderName: "Budi"}
	group := Incoming{ChatID: "123@g.us", IsGroup: true, ChatName: "PO", SenderName: "Budi"}
	cfg := ReplyConfig{Mode: "send", Confirm: "{items}", Thanks: "Makasih"}

	r := NewReplier(cfg, nil)
	r.AskToConfirm(group, "ord_1", order)
	r.Thank(group, order)
	r.AskToConfirm(private, "ord_2", Order{IsOrder: true}) // nothing to confirm
	if len(r.queue) != 0 {
		t.Errorf("queued %d replies, want none", len(r.queue))
	}
	r.AskToConfirm(private, "ord_3", order)
	if len(r.queue) != 1 {
		t.Errorf("queued %d replies, want 1", len(r.queue))
	}

	cfg.Mode = "preview"
	r = NewReplier(cfg, nil)
	r.AskToConfirm(private, "ord_4", order)
	if len(r.queue) != 0 {
		t.Error("preview mode queued a reply")
	}
}

func TestReplierPacing(t *testing.T) {
	r := NewReplier(ReplyConfig{Mode: "send", Confirm: "{items}", MinGap: 40 * time.Millisecond, MaxPerHour: 2}, openTestStore(t))
	r.window = 300 * time.Millisecond // stands in for the hour
	var outbox sentMessages
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx, outbox.send)

	order := Order{IsOrder: true, Items: []OrderItem{{Product: "Dimsum", Quantity: 1}}}
	for i, chat := range []string{"a@s.whatsapp.net", "b@s.whatsapp.net", "c@s.whatsapp.net"} {
		r.AskToConfirm(Incoming{ChatID: chat, SenderID: chat}, "ord_"+string(rune('1'+i)), order)
	}
	waitFor(t, "three replies", func() bool { return outbox.count() == 3 })
	first, second, third := outbox.get(0).at, outbox.get(1).at, outbox.get(2).at
	if gap := second.Sub(first); gap < 40*time.Millisecond {
		t.Errorf("second reply %s after the first, want at least reply.min_gap", gap)
	}
	if gap := third.Sub(first); gap < 300*time.Millisecond {
		t.Errorf("third reply %s after the first, want it held back by reply.max_per_hour", gap)
	}
}

func TestReplierRetriesAndRecordsTheRequest(t *testing.T) {
	store := openTestStore(t)
	r := NewReplier(ReplyConfig{Mode: "send", Confirm: "{items}", MinGap: time.Millisecond, MaxPerHour: 10}, store)
	r.retryIn = time.Millisecond
	outbox := sentMessages{failFirst: 2}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx, outbox.send)

	to := budiSays("1", "pesan 2")
	r.AskToConfirm(to, "ord_1", Order{IsOrder: true, Items: []OrderItem{{Product: "Dimsum", Quantity: 2}}})
	waitFor(t, "the reply", func() bool { return outbox.count() == 1 })
	waitFor(t, "the request to be recorded", func() bool {
		w, _ := store.WaitingConfirmation(ctx, to.ChatID, to.SenderID, time.Now().Add(-time.Minute))
		return w != nil && w.ID == "ord_1" && w.Order.Items[0].Quantity == 2
	})
	outbox.mu.Lock()
	defer outbox.mu.Unlock()
	if outbox.calls != 3 {
		t.Errorf("tried %d times, want 3", outbox.calls)
	}
}
