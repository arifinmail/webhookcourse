package main

import (
	"testing"
	"time"
)

func TestSamePhone(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0812-3456-7890", "6281234567890", true},
		{"+62 812 3456 7890", "6281234567890", true},
		{"6281234567890", "6281234567890", true},
		{"0812-3456-7890", "6281234567891", false},
		{"", "6281234567890", false},
		{"12345", "6212345", false}, // too short to compare safely
	}
	for _, c := range cases {
		if got := samePhone(c.a, c.b); got != c.want {
			t.Errorf("samePhone(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestAllow(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	f := FilterConfig{
		PrivateChats:  true,
		Groups:        []string{"PO Oktober", "120363000000000001@g.us"},
		IgnoreNumbers: []string{"0812-0000-0000"},
		SkipPhrases:   []string{"ok", "makasih", "👍"},
		MinLength:     2,
		MaxAge:        24 * time.Hour,
	}
	customer := Incoming{ChatID: "6281111111111@s.whatsapp.net", SenderPhone: "6281111111111", Time: now, Text: "pesan 2 dimsum"}
	group := func(id, name string) Incoming {
		m := customer
		m.IsGroup, m.ChatID, m.ChatName = true, id, name
		return m
	}
	with := func(edit func(*Incoming)) Incoming {
		m := customer
		edit(&m)
		return m
	}

	cases := []struct {
		name string
		m    Incoming
		want bool
	}{
		{"customer in private chat", customer, true},
		{"watched group by name, any case", group("120363000000000009@g.us", "po oktober"), true},
		{"watched group by id", group("120363000000000001@g.us", "Renamed group"), true},
		{"other group", group("120363000000000002@g.us", "Family"), false},
		{"ignored number", with(func(m *Incoming) { m.SenderPhone = "6281200000000" }), false},
		{"no text", with(func(m *Incoming) { m.Text = "  " }), false},
		{"too short", with(func(m *Incoming) { m.Text = "y" }), false},
		{"small talk", with(func(m *Incoming) { m.Text = "Ok!!" }), false},
		{"emoji only", with(func(m *Incoming) { m.Text = "👍" }), false},
		{"small talk inside a longer message", with(func(m *Incoming) { m.Text = "ok, pesan 2 lagi" }), true},
		{"too old", with(func(m *Incoming) { m.Time = now.Add(-25 * time.Hour) }), false},
	}
	for _, c := range cases {
		if got, reason := f.Allow(c.m, now); got != c.want {
			t.Errorf("%s: Allow = %v (%s), want %v", c.name, got, reason, c.want)
		}
	}

	f.PrivateChats = false
	if ok, _ := f.Allow(customer, now); ok {
		t.Error("private chat allowed with private_chats off")
	}
}

func TestWorthSending(t *testing.T) {
	batch := []Incoming{{Text: "Halo kak"}, {Text: "Mau PESAN 2 ya"}}
	if !(FilterConfig{}).WorthSending(batch) {
		t.Error("without keywords every batch should be sent")
	}
	if !(FilterConfig{Keywords: []string{"pesan"}}).WorthSending(batch) {
		t.Error("keyword match should be case-insensitive and look at every message")
	}
	if (FilterConfig{Keywords: []string{"order", "beli"}}).WorthSending(batch) {
		t.Error("batch without keywords was sent")
	}
}
