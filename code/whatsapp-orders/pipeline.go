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
	Extract(ctx context.Context, batch []Incoming) (Order, string, error)
}

// Pipeline wires the steps together: filter -> save -> batch -> read with Claude -> deliver.
type Pipeline struct {
	ctx       context.Context
	filter    FilterConfig
	store     *Store
	batcher   *Batcher
	reader    orderReader
	deliverer *Deliverer
	verbose   bool
	retryIn   time.Duration
}

// A batch Claude gives no usable answer for is tried this many times before its
// messages are marked "error".
const maxReadAttempts = 3

func NewPipeline(ctx context.Context, cfg Config, store *Store, reader orderReader, deliverer *Deliverer, verbose bool) *Pipeline {
	p := &Pipeline{
		ctx:       ctx,
		filter:    cfg.Filter,
		store:     store,
		reader:    reader,
		deliverer: deliverer,
		verbose:   verbose,
		retryIn:   time.Minute,
	}
	p.batcher = NewBatcher(cfg.Batching.QuietPeriod, cfg.Batching.MaxWait, func(batch []Incoming) { p.read(batch, 1) })
	return p
}

// Handle is called for every incoming WhatsApp message.
func (p *Pipeline) Handle(m Incoming) {
	if ok, reason := p.filter.Allow(m, time.Now()); !ok {
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

func (p *Pipeline) read(batch []Incoming, attempt int) {
	sort.SliceStable(batch, func(i, j int) bool { return batch[i].Time.Before(batch[j].Time) })
	from := who(batch[0])

	if !p.filter.WorthSending(batch) {
		p.mark(batch, "skipped", "")
		if p.verbose {
			log.Printf("skipped messages from %s: no word from filter.keywords", from)
		}
		return
	}

	order, model, err := p.reader.Extract(p.ctx, batch)
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

	if !order.IsOrder {
		p.mark(batch, "not_order", "")
		log.Printf("messages from %s are not an order", from)
		return
	}
	ev := newOrderEvent(batch, order, model)
	if err := p.deliverer.Enqueue(p.ctx, ev); err != nil {
		log.Printf("could not save the order from %s: %v", from, err)
		return
	}
	p.mark(batch, "ordered", ev.ID)
	log.Printf("order %s from %s: %s", ev.ID, from, order.Summary)
}

func (p *Pipeline) mark(batch []Incoming, status, orderID string) {
	if err := p.store.MarkMessages(p.ctx, batch, status, orderID); err != nil {
		log.Printf("could not update message status: %v", err)
	}
}

func newOrderEvent(batch []Incoming, order Order, model string) OrderEvent {
	first := batch[0]
	ev := OrderEvent{
		ID:        newOrderID(),
		CreatedAt: time.Now().UTC(),
		Source: OrderSource{
			ChatType:    "private",
			ChatID:      first.ChatID,
			SenderPhone: first.SenderPhone,
			SenderName:  first.SenderName,
		},
		Order: order,
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

func newOrderID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return "ord_" + time.Now().UTC().Format("20060102T150405") + "_" + hex.EncodeToString(b)
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
