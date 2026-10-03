package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"
)

type orderReader interface {
	Extract(ctx context.Context, batch []Incoming, previous *Order) (Reading, error)
}

// Pipeline wires the steps together: filter -> save -> batch -> read with Claude ->
// send to your system, and ask the customer to confirm when replies are on.
type Pipeline struct {
	ctx           context.Context
	filter        FilterConfig
	store         *Store
	batcher       *Batcher
	reader        orderReader
	deliverer     *Deliverer
	replier       *Replier
	confirmWindow time.Duration
	verbose       bool
	retryIn       time.Duration
}

// A batch Claude gives no usable answer for is tried this many times before its
// messages are marked "error".
const maxReadAttempts = 3

func NewPipeline(ctx context.Context, cfg Config, store *Store, reader orderReader, deliverer *Deliverer, replier *Replier, verbose bool) *Pipeline {
	p := &Pipeline{
		ctx:           ctx,
		filter:        cfg.Filter,
		store:         store,
		reader:        reader,
		deliverer:     deliverer,
		replier:       replier,
		confirmWindow: cfg.Reply.ConfirmWindow,
		verbose:       verbose,
		retryIn:       time.Minute,
	}
	p.batcher = NewBatcher(cfg.Batching.QuietPeriod, cfg.Batching.MaxWait, func(batch []Incoming) { p.read(batch, 1) })
	return p
}

// Handle is called for every incoming WhatsApp message.
func (p *Pipeline) Handle(m Incoming) {
	ok, reason := p.filter.AllowSender(m, time.Now())
	if ok {
		// Someone answering a confirmation request often just writes "ok", which the
		// text rules would drop.
		if ok, reason = p.filter.AllowText(m.Text); !ok && p.waitingOrder(m) != nil {
			ok = true
		}
	}
	if !ok {
		if p.verbose {
			log.Printf("skipped a message from %s: %s", who(m), reason)
		}
		return
	}
	isNew, err := p.store.SaveMessage(p.ctx, m)
	if err != nil {
		log.Printf("could not save a message from %s: %v", who(m), err)
		return
	}
	if isNew {
		p.batcher.Add(m)
	}
}

// Resume queues the messages that were still waiting when the program last stopped.
func (p *Pipeline) Resume() error {
	pending, err := p.store.PendingMessages(p.ctx)
	if err != nil {
		return err
	}
	for _, m := range pending {
		p.batcher.Add(m)
	}
	if len(pending) > 0 {
		log.Printf("picked up %d message(s) still waiting from last time", len(pending))
	}
	return nil
}

// waitingOrder returns the order this person was asked to confirm in this chat and
// hasn't answered yet, or nil.
func (p *Pipeline) waitingOrder(m Incoming) *waitingOrder {
	w, err := p.store.WaitingConfirmation(p.ctx, m.ChatID, m.SenderID, time.Now().Add(-p.confirmWindow))
	if err != nil {
		log.Printf("could not look up confirmations: %v", err)
		return nil
	}
	return w
}

func (p *Pipeline) read(batch []Incoming, attempt int) {
	sort.SliceStable(batch, func(i, j int) bool { return batch[i].Time.Before(batch[j].Time) })
	last := batch[len(batch)-1]
	from := who(last)
	waiting := p.waitingOrder(last)

	if waiting == nil && !p.filter.WorthSending(batch) {
		p.mark(batch, "skipped", "")
		if p.verbose {
			log.Printf("skipped messages from %s: no word from filter.keywords", from)
		}
		return
	}

	var previous *Order
	if waiting != nil {
		previous = &waiting.Order
	}
	reading, err := p.reader.Extract(p.ctx, batch, previous)
	if err != nil {
		if p.ctx.Err() != nil {
			return // shutting down; the messages stay pending for next time
		}
		var bad badAnswer
		if errors.As(err, &bad) && attempt >= maxReadAttempts {
			log.Printf("gave up reading messages from %s: %v", from, err)
			p.mark(batch, "error", "")
			return
		}
		// Problems reaching Claude (no internet, overload, a wrong API key) are retried
		// until they are fixed, so no order gets lost.
		wait := min(p.retryIn<<min(attempt-1, 5), 30*time.Minute)
		log.Printf("could not read messages from %s, trying again in %s: %v", from, wait, err)
		time.AfterFunc(wait, func() { p.read(batch, attempt+1) })
		return
	}

	replaces := ""
	if waiting != nil {
		switch reading.AboutPrevious {
		case "confirms":
			if p.answered(batch, waiting, "order.confirmed", "confirmed", reading.Model) {
				p.replier.Thank(last, waiting.Order)
			}
			return
		case "cancels":
			p.answered(batch, waiting, "order.cancelled", "cancelled", reading.Model)
			return
		case "changes":
			if reading.Order.IsOrder {
				replaces = waiting.ID
			}
		}
	}

	if !reading.Order.IsOrder {
		p.mark(batch, "not_order", "")
		log.Printf("messages from %s are not an order", from)
		return
	}
	ev := newEvent("order.created", "ord", batch, reading.Model)
	ev.OrderID = ev.ID
	ev.Replaces = replaces
	ev.Order = &reading.Order
	if err := p.deliverer.Enqueue(p.ctx, ev); err != nil {
		log.Printf("could not save the order from %s: %v", from, err)
		return
	}
	p.mark(batch, "ordered", ev.OrderID)
	if replaces != "" {
		p.closeConfirmation(replaces, "changed")
		log.Printf("order %s from %s replaces %s: %s", ev.OrderID, from, replaces, reading.Order.Summary)
	} else {
		log.Printf("order %s from %s: %s", ev.OrderID, from, reading.Order.Summary)
	}
	p.replier.AskToConfirm(last, ev.OrderID, reading.Order)
}

// answered sends your system the customer's answer to a confirmation request.
func (p *Pipeline) answered(batch []Incoming, waiting *waitingOrder, eventType, state, model string) bool {
	ev := newEvent(eventType, "evt", batch, model)
	ev.OrderID = waiting.ID
	if err := p.deliverer.Enqueue(p.ctx, ev); err != nil {
		log.Printf("could not save the answer from %s: %v", who(batch[0]), err)
		return false
	}
	p.closeConfirmation(waiting.ID, state)
	p.mark(batch, state, waiting.ID)
	log.Printf("%s %s order %s", who(batch[0]), state, waiting.ID)
	return true
}

func (p *Pipeline) closeConfirmation(orderID, state string) {
	if err := p.store.CloseConfirmation(p.ctx, orderID, state); err != nil {
		log.Printf("could not update the confirmation of %s: %v", orderID, err)
	}
}

func (p *Pipeline) mark(batch []Incoming, status, orderID string) {
	if err := p.store.MarkMessages(p.ctx, batch, status, orderID); err != nil {
		log.Printf("could not update message status: %v", err)
	}
}

// newEvent fills in what every event has: a new id, where the messages came from, and
// the messages themselves.
func newEvent(eventType, idPrefix string, batch []Incoming, model string) OrderEvent {
	first := batch[0]
	ev := OrderEvent{
		ID:        newID(idPrefix),
		Type:      eventType,
		CreatedAt: time.Now().UTC(),
		Source: OrderSource{
			ChatType:    "private",
			ChatID:      first.ChatID,
			SenderPhone: first.SenderPhone,
			SenderName:  first.SenderName,
		},
		Model: model,
	}
	if first.IsGroup {
		ev.Source.ChatType = "group"
		ev.Source.ChatName = first.ChatName
	}
	for _, m := range batch {
		ev.Messages = append(ev.Messages, SourceMessage{ID: m.ID, Time: m.Time.UTC(), Text: m.Text})
	}
	return ev
}

func newID(prefix string) string {
	b := make([]byte, 4)
	rand.Read(b)
	return prefix + "_" + time.Now().UTC().Format("20060102T150405") + "_" + hex.EncodeToString(b)
}

// who describes the sender for log lines, without the message text.
func who(m Incoming) string {
	name := m.SenderName
	if name == "" {
		name = m.SenderPhone
	}
	if name == "" {
		name = "unknown sender"
	}
	if m.IsGroup {
		return fmt.Sprintf("%s in %q", name, m.ChatName)
	}
	return name
}
