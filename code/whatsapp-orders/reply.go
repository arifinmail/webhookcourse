package main

import (
	"context"
	"log"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"
)

// Replier sends confirmation messages to customers, only in private chats, never in
// groups. It sends slowly, at least reply.min_gap apart and at most reply.max_per_hour,
// to stay clear of WhatsApp's spam detection. In "preview" mode it only logs what it
// would send.
type Replier struct {
	cfg     ReplyConfig
	store   *Store
	queue   chan outgoing
	window  time.Duration // reply.max_per_hour counts the sends in this window
	retryIn time.Duration
	sent    []time.Time
}

type outgoing struct {
	to   Incoming // the customer's last message: which chat, which person
	text string
	// Set when the message asks the customer to confirm this order.
	orderID string
	order   Order
}

func NewReplier(cfg ReplyConfig, store *Store) *Replier {
	return &Replier{
		cfg:     cfg,
		store:   store,
		queue:   make(chan outgoing, 200),
		window:  time.Hour,
		retryIn: 30 * time.Second,
	}
}

// AskToConfirm sends the customer a summary of their order and asks them to confirm it.
func (r *Replier) AskToConfirm(to Incoming, orderID string, order Order) {
	if to.IsGroup || len(order.Items) == 0 {
		return
	}
	r.add(outgoing{to: to, text: confirmText(r.cfg, order, to.SenderName), orderID: orderID, order: order})
}

// Thank sends reply.thanks, if set, after a customer confirmed their order.
func (r *Replier) Thank(to Incoming, order Order) {
	if to.IsGroup || strings.TrimSpace(r.cfg.Thanks) == "" {
		return
	}
	r.add(outgoing{to: to, text: fillTemplate(r.cfg.Thanks, order, to.SenderName, "")})
}

func (r *Replier) add(m outgoing) {
	switch r.cfg.Mode {
	case "preview":
		log.Printf("reply preview for %s (not sent, reply.mode is preview):\n%s", who(m.to), m.text)
	case "send":
		select {
		case r.queue <- m:
		default:
			log.Printf("too many replies waiting; not sending one to %s", who(m.to))
		}
	}
}

// Run sends the queued replies through send until ctx is cancelled.
func (r *Replier) Run(ctx context.Context, send func(ctx context.Context, chatID, text string) error) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-r.queue:
			if !r.waitTurn(ctx) {
				return
			}
			r.deliver(ctx, m, send)
		}
	}
}

func (r *Replier) deliver(ctx context.Context, m outgoing, send func(ctx context.Context, chatID, text string) error) {
	for attempt := 1; ; attempt++ {
		err := send(ctx, m.to.ChatID, m.text)
		if err == nil {
			break
		}
		if attempt == 3 || !sleep(ctx, r.retryIn) {
			log.Printf("could not send a reply to %s: %v", who(m.to), err)
			return
		}
	}
	r.sent = append(r.sent, time.Now())
	log.Printf("sent a reply to %s", who(m.to))
	if m.orderID != "" {
		if err := r.store.SaveConfirmation(ctx, m.orderID, m.to.ChatID, m.to.SenderID, m.order); err != nil {
			log.Printf("could not save the confirmation request for %s: %v", m.orderID, err)
		}
	}
}

// waitTurn waits until the next reply may go out, and reports false if ctx ends first.
func (r *Replier) waitTurn(ctx context.Context) bool {
	if n := len(r.sent); n > 0 {
		// A little randomness, so replies don't go out on an exact beat.
		gap := r.cfg.MinGap + rand.N(r.cfg.MinGap/4+1)
		if !sleep(ctx, time.Until(r.sent[n-1].Add(gap))) {
			return false
		}
	}
	cutoff := time.Now().Add(-r.window)
	for len(r.sent) > 0 && r.sent[0].Before(cutoff) {
		r.sent = r.sent[1:]
	}
	if len(r.sent) >= r.cfg.MaxPerHour {
		wait := time.Until(r.sent[0].Add(r.window))
		log.Printf("reached reply.max_per_hour, next reply in %s", wait.Round(time.Second))
		return sleep(ctx, wait)
	}
	return true
}

// sleep waits for d and reports false if ctx ends first.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// confirmText fills in reply.confirm for an order.
func confirmText(cfg ReplyConfig, order Order, whatsappName string) string {
	missing := ""
	if len(order.MissingInfo) > 0 {
		missing = strings.ReplaceAll(cfg.Missing, "{list}", strings.Join(order.MissingInfo, ", "))
	}
	return fillTemplate(cfg.Confirm, order, whatsappName, missing)
}

// fillTemplate replaces {name}, {items}, {missing}, {summary}, {address} and {time}.
func fillTemplate(template string, order Order, whatsappName, missing string) string {
	name := order.CustomerName
	if name == "" {
		name = whatsappName
	}
	var items []string
	for _, it := range order.Items {
		parts := []string{"-"}
		if it.Quantity > 0 {
			parts = append(parts, strconv.FormatFloat(it.Quantity, 'f', -1, 64))
		}
		if it.Unit != "" {
			parts = append(parts, it.Unit)
		}
		line := strings.Join(append(parts, it.Product), " ")
		if it.Notes != "" {
			line += " (" + it.Notes + ")"
		}
		items = append(items, line)
	}
	text := strings.NewReplacer(
		"{name}", name,
		"{items}", strings.Join(items, "\n"),
		"{missing}", missing,
		"{summary}", order.Summary,
		"{address}", order.DeliveryAddress,
		"{time}", order.DeliveryTime,
	).Replace(template)
	return tidy(text)
}

// tidy cleans up after empty placeholders: extra spaces, spaces before commas, and
// runs of blank lines.
func tidy(text string) string {
	var out []string
	blank := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.ReplaceAll(strings.Join(strings.Fields(line), " "), " ,", ",")
		if line == "" && blank {
			continue
		}
		blank = line == ""
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
