package main

import (
	"strings"
	"time"
	"unicode"
)

// Incoming is one WhatsApp message, reduced to what the order pipeline needs.
type Incoming struct {
	ID          string
	ChatID      string
	ChatName    string
	IsGroup     bool
	SenderID    string
	SenderPhone string
	SenderName  string
	Time        time.Time
	Text        string
}

// Key groups messages from the same person in the same chat into one batch.
func (m Incoming) Key() string { return m.ChatID + "|" + m.SenderID }

// Allow decides whether a single message may be sent to the AI at all.
// When it says no, it also says why (without repeating the message text).
func (f FilterConfig) Allow(m Incoming, now time.Time) (bool, string) {
	if ok, reason := f.AllowSender(m, now); !ok {
		return false, reason
	}
	return f.AllowText(m.Text)
}

// AllowSender applies the rules about who wrote the message, where and when.
func (f FilterConfig) AllowSender(m Incoming, now time.Time) (bool, string) {
	switch {
	case strings.TrimSpace(m.Text) == "":
		return false, "no text (sticker, photo without caption, voice note, ...)"
	case f.MaxAge > 0 && now.Sub(m.Time) > f.MaxAge:
		return false, "older than filter.max_age"
	case m.IsGroup && !f.watchesGroup(m.ChatID, m.ChatName):
		return false, "group is not in filter.groups"
	case !m.IsGroup && !f.PrivateChats:
		return false, "filter.private_chats is off"
	case anyPhone(f.IgnoreNumbers, m.SenderPhone):
		return false, "sender is in filter.ignore_numbers"
	case len(f.OnlyNumbers) > 0 && !anyPhone(f.OnlyNumbers, m.SenderPhone):
		return false, "sender is not in filter.only_numbers"
	}
	return true, ""
}

// AllowText applies the rules about the text itself. The pipeline lets customers who
// were just asked to confirm an order skip these, since their answer is often "ok".
func (f FilterConfig) AllowText(text string) (bool, string) {
	text = strings.TrimSpace(text)
	switch {
	case len([]rune(text)) < f.MinLength:
		return false, "shorter than filter.min_length"
	case f.isSkipPhrase(text):
		return false, "matches filter.skip_phrases"
	}
	return true, ""
}

// WorthSending applies filter.keywords to a whole batch: when keywords are set,
// at least one message in the batch has to contain one of them.
func (f FilterConfig) WorthSending(batch []Incoming) bool {
	if len(f.Keywords) == 0 {
		return true
	}
	for _, m := range batch {
		text := strings.ToLower(m.Text)
		for _, k := range f.Keywords {
			if k = strings.ToLower(strings.TrimSpace(k)); k != "" && strings.Contains(text, k) {
				return true
			}
		}
	}
	return false
}

func (f FilterConfig) watchesGroup(id, name string) bool {
	for _, g := range f.Groups {
		g = strings.TrimSpace(g)
		if g != "" && (g == id || strings.EqualFold(g, strings.TrimSpace(name))) {
			return true
		}
	}
	return false
}

func anyPhone(numbers []string, phone string) bool {
	for _, n := range numbers {
		if samePhone(n, phone) {
			return true
		}
	}
	return false
}

func (f FilterConfig) isSkipPhrase(text string) bool {
	text = normalizePhrase(text)
	for _, p := range f.SkipPhrases {
		if p = normalizePhrase(p); p != "" && p == text {
			return true
		}
	}
	return false
}

// normalizePhrase lowercases text and drops punctuation and spacing, so "Ok!!" matches "ok".
func normalizePhrase(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsPunct(r) || unicode.IsSpace(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, s)
}

// samePhone compares phone numbers written in different ways, so "0812-3456-7890",
// "+62 812 3456 7890" and "6281234567890" all match each other.
func samePhone(a, b string) bool {
	a = strings.TrimLeft(digits(a), "0")
	b = strings.TrimLeft(digits(b), "0")
	if len(a) > len(b) {
		a, b = b, a
	}
	return len(a) >= 8 && strings.HasSuffix(b, a)
}

func digits(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
}
