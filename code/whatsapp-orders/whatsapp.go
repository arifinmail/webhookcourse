package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// WhatsApp links to your account as a linked device, like WhatsApp Web. It sends
// nothing except the confirmation messages you turn on with reply.mode: no read
// receipts, no "online" status.
type WhatsApp struct {
	client     *whatsmeow.Client
	handle     func(Incoming)
	groupNames sync.Map // group JID -> group name

	loggedOut     chan struct{}
	loggedOutOnce sync.Once
}

// ConnectWhatsApp opens the saved login in dataDir, or links a new device first:
// by QR code, or by a code typed on the phone when pairPhone is set.
func ConnectWhatsApp(ctx context.Context, dataDir, pairPhone string, handle func(Incoming)) (*WhatsApp, error) {
	// The name shown under Linked devices on your phone.
	store.DeviceProps.Os = proto.String("Order reader")

	dsn := "file:" + filepath.Join(dataDir, "whatsapp.db") + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	container, err := sqlstore.New(ctx, "sqlite", dsn, waLog.Noop)
	if err != nil {
		return nil, fmt.Errorf("opening the WhatsApp login store: %w", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, err
	}
	wa := &WhatsApp{
		client:    whatsmeow.NewClient(device, waLog.Stdout("WhatsApp", "WARN", true)),
		handle:    handle,
		loggedOut: make(chan struct{}),
	}
	wa.client.AddEventHandler(wa.onEvent)

	if wa.client.Store.ID != nil {
		if err := wa.client.Connect(); err != nil {
			return nil, fmt.Errorf("connecting to WhatsApp: %w", err)
		}
		return wa, nil
	}
	if err := wa.link(ctx, pairPhone); err != nil {
		return nil, err
	}
	return wa, nil
}

func (wa *WhatsApp) link(ctx context.Context, pairPhone string) error {
	qr, err := wa.client.GetQRChannel(ctx)
	if err != nil {
		return err
	}
	if err := wa.client.Connect(); err != nil {
		return fmt.Errorf("connecting to WhatsApp: %w", err)
	}
	askedForCode := false
	for item := range qr {
		switch item.Event {
		case whatsmeow.QRChannelEventCode:
			if pairPhone == "" {
				fmt.Println("\nOn your phone: WhatsApp > Linked devices > Link a device, then scan this QR code:")
				qrterminal.GenerateHalfBlock(item.Code, qrterminal.L, os.Stdout)
			} else if !askedForCode {
				// The first QR event means the connection is ready to ask for a code.
				askedForCode = true
				code, err := wa.client.PairPhone(ctx, digits(pairPhone), true, whatsmeow.PairClientChrome, "Chrome (Linux)")
				if err != nil {
					wa.client.Disconnect()
					return fmt.Errorf("getting a link code: %w", err)
				}
				fmt.Printf("\nOn your phone: WhatsApp > Linked devices > Link a device > Link with phone number instead.\nEnter this code: %s\n\n", code)
			}
		case whatsmeow.QRChannelSuccess.Event:
			fmt.Println("Linked. Next time just start the program again; no code needed.")
			return nil
		default:
			wa.client.Disconnect()
			return fmt.Errorf("linking WhatsApp failed (%s); start the program again to get a new code", item.Event)
		}
	}
	return nil
}

func (wa *WhatsApp) Disconnect() { wa.client.Disconnect() }

// Send sends a text message to a chat. Only the Replier uses it.
func (wa *WhatsApp) Send(ctx context.Context, chatID, text string) error {
	to, err := types.ParseJID(chatID)
	if err != nil {
		return err
	}
	_, err = wa.client.SendMessage(ctx, to, &waE2E.Message{Conversation: proto.String(text)})
	return err
}

// LoggedOut is closed when the phone removes this linked device.
func (wa *WhatsApp) LoggedOut() <-chan struct{} { return wa.loggedOut }

func (wa *WhatsApp) onEvent(evt any) {
	switch v := evt.(type) {
	case *events.Message:
		if m, ok := wa.toIncoming(v); ok {
			wa.handle(m)
		}
	case *events.GroupInfo:
		if v.Name != nil {
			wa.groupNames.Store(v.JID.String(), v.Name.Name)
		}
	case *events.Connected:
		log.Println("Connected to WhatsApp, waiting for messages.")
	case *events.LoggedOut:
		log.Println("WhatsApp removed this linked device. Start the program again to link it.")
		wa.loggedOutOnce.Do(func() { close(wa.loggedOut) })
	}
}

func (wa *WhatsApp) toIncoming(v *events.Message) (Incoming, bool) {
	info := v.Info
	if info.IsFromMe || info.Chat.Server == types.BroadcastServer || info.Chat.Server == types.NewsletterServer {
		return Incoming{}, false
	}
	text := messageText(v.Message)
	if v.IsEdit && text != "" {
		text = "[edited] " + text
	}
	ctx := context.Background()
	m := Incoming{
		ID:          info.ID,
		ChatID:      info.Chat.String(),
		IsGroup:     info.IsGroup,
		SenderID:    info.Sender.ToNonAD().String(),
		SenderPhone: wa.phoneOf(ctx, info),
		SenderName:  wa.nameOf(ctx, info),
		Time:        info.Timestamp,
		Text:        text,
	}
	if m.IsGroup {
		m.ChatName = wa.groupName(ctx, info.Chat)
	}
	return m, true
}

// phoneOf finds the sender's phone number. WhatsApp increasingly hides numbers behind
// "LID" ids, especially in groups; whatsmeow keeps a table to map them back.
func (wa *WhatsApp) phoneOf(ctx context.Context, info types.MessageInfo) string {
	for _, jid := range []types.JID{info.Sender, info.SenderAlt} {
		if jid.Server == types.DefaultUserServer {
			return jid.User
		}
	}
	if info.Sender.Server == types.HiddenUserServer {
		if pn, err := wa.client.Store.LIDs.GetPNForLID(ctx, info.Sender.ToNonAD()); err == nil && pn.Server == types.DefaultUserServer {
			return pn.User
		}
	}
	return ""
}

// nameOf prefers the name saved in your phone's contacts, then the sender's own WhatsApp name.
func (wa *WhatsApp) nameOf(ctx context.Context, info types.MessageInfo) string {
	if c, err := wa.client.Store.Contacts.GetContact(ctx, info.Sender.ToNonAD()); err == nil && c.Found {
		for _, name := range []string{c.FullName, c.FirstName, c.BusinessName} {
			if name != "" {
				return name
			}
		}
	}
	return info.PushName
}

func (wa *WhatsApp) groupName(ctx context.Context, jid types.JID) string {
	if name, ok := wa.groupNames.Load(jid.String()); ok {
		return name.(string)
	}
	info, err := wa.client.GetGroupInfo(ctx, jid)
	if err != nil {
		log.Printf("could not look up the name of group %s: %v", jid, err)
		return ""
	}
	wa.groupNames.Store(jid.String(), info.Name)
	return info.Name
}

// messageText returns the text of a message, including captions of photos, videos
// and documents. Stickers, voice notes and reactions have no text.
func messageText(msg *waE2E.Message) string {
	if msg == nil {
		return ""
	}
	for _, text := range []string{
		msg.GetConversation(),
		msg.GetExtendedTextMessage().GetText(),
		msg.GetImageMessage().GetCaption(),
		msg.GetVideoMessage().GetCaption(),
		msg.GetDocumentMessage().GetCaption(),
	} {
		if text != "" {
			return text
		}
	}
	// An edit arrives as a protocol message that wraps the new version.
	return messageText(msg.GetProtocolMessage().GetEditedMessage())
}
