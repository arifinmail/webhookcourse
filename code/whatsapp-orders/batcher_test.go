package main

import (
	"sync"
	"testing"
	"time"
)

type flushes struct {
	mu  sync.Mutex
	got [][]Incoming
}

func (f *flushes) add(b []Incoming) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, b)
}

// waitFor polls until cond is true or fails the test after 3 seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestBatcherGroupsPerPersonAndChat(t *testing.T) {
	var f flushes
	b := NewBatcher(40*time.Millisecond, time.Second, f.add)
	b.Add(Incoming{ID: "a1", ChatID: "chat1", SenderID: "alice", Text: "hi"})
	b.Add(Incoming{ID: "b1", ChatID: "chat1", SenderID: "bob", Text: "hello"})
	time.Sleep(10 * time.Millisecond)
	b.Add(Incoming{ID: "a2", ChatID: "chat1", SenderID: "alice", Text: "2 please"})

	waitFor(t, "two batches", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.got) == 2 })
	time.Sleep(60 * time.Millisecond) // no extra flushes may follow
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.got) != 2 {
		t.Fatalf("got %d batches, want 2", len(f.got))
	}
	sizes := map[string]int{}
	for _, batch := range f.got {
		sizes[batch[0].SenderID] = len(batch)
	}
	if sizes["alice"] != 2 || sizes["bob"] != 1 {
		t.Errorf("batch sizes = %v, want alice:2 bob:1", sizes)
	}
}

func TestBatcherMaxWait(t *testing.T) {
	var f flushes
	b := NewBatcher(100*time.Millisecond, 150*time.Millisecond, f.add)
	for i := range 10 { // someone who keeps typing for ~400ms
		b.Add(Incoming{ID: string(rune('a' + i)), ChatID: "chat", SenderID: "alice"})
		time.Sleep(40 * time.Millisecond)
	}
	total := func() int {
		f.mu.Lock()
		defer f.mu.Unlock()
		n := 0
		for _, batch := range f.got {
			n += len(batch)
		}
		return n
	}
	waitFor(t, "all messages", func() bool { return total() == 10 })
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.got) < 2 {
		t.Errorf("max_wait never cut the batch: got %d batch(es)", len(f.got))
	}
}
