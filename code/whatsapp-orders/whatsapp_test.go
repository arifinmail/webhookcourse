package main

import (
	"context"
	"path/filepath"
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// The WhatsApp login store must work with the pure-Go SQLite driver, so the program
// builds on Windows and macOS without a C compiler.
func TestLoginStoreOpens(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "whatsapp.db") + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	container, err := sqlstore.New(context.Background(), "sqlite", dsn, waLog.Noop)
	if err != nil {
		t.Fatal(err)
	}
	device, err := container.GetFirstDevice(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if device.ID != nil {
		t.Error("a fresh store should have no linked device yet")
	}
}

func TestMessageText(t *testing.T) {
	cases := []struct {
		name string
		msg  *waE2E.Message
		want string
	}{
		{"plain", &waE2E.Message{Conversation: proto.String("pesan 2")}, "pesan 2"},
		{"reply or link", &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String("yang ini 3")}}, "yang ini 3"},
		{"photo caption", &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("mau ini")}}, "mau ini"},
		{"edit", &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
			EditedMessage: &waE2E.Message{Conversation: proto.String("jadi 4")},
		}}, "jadi 4"},
		{"sticker", &waE2E.Message{StickerMessage: &waE2E.StickerMessage{}}, ""},
		{"nothing", nil, ""},
	}
	for _, c := range cases {
		if got := messageText(c.msg); got != c.want {
			t.Errorf("%s: messageText = %q, want %q", c.name, got, c.want)
		}
	}
}
