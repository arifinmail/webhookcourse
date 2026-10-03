package main

import (
	"sync"
	"time"
)

// Batcher collects messages per person per chat and hands them over together once
// that person has been quiet for a while, because customers often split one order
// over several messages ("hi", "2 of these", "send to ...").
type Batcher struct {
	quiet, maxWait time.Duration
	flush          func([]Incoming)

	mu      sync.Mutex
	pending map[string]*batch
}

type batch struct {
	msgs  []Incoming
	first time.Time
	timer *time.Timer
}

func NewBatcher(quiet, maxWait time.Duration, flush func([]Incoming)) *Batcher {
	return &Batcher{quiet: quiet, maxWait: maxWait, flush: flush, pending: map[string]*batch{}}
}

// Add queues a message. Its batch is flushed after the quiet period, or once the
// batch is maxWait old if the person keeps writing.
func (b *Batcher) Add(m Incoming) {
	b.mu.Lock()
	defer b.mu.Unlock()

	key := m.Key()
	p := b.pending[key]
	if p == nil {
		p = &batch{first: time.Now()}
		b.pending[key] = p
	}
	p.msgs = append(p.msgs, m)
	if p.timer != nil {
		p.timer.Stop()
	}
	wait := min(b.quiet, max(b.maxWait-time.Since(p.first), 0))
	p.timer = time.AfterFunc(wait, func() { b.fire(key, p) })
}

func (b *Batcher) fire(key string, p *batch) {
	b.mu.Lock()
	if b.pending[key] != p {
		// Already flushed by an earlier timer.
		b.mu.Unlock()
		return
	}
	delete(b.pending, key)
	msgs := p.msgs
	b.mu.Unlock()

	b.flush(msgs)
}
