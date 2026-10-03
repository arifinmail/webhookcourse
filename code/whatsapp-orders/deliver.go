package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

// OrderEvent is the JSON body your system receives for every order.
type OrderEvent struct {
	ID        string          `json:"id"`
	CreatedAt time.Time       `json:"created_at"`
	Source    OrderSource     `json:"source"`
	Messages  []SourceMessage `json:"messages"`
	Order     Order           `json:"order"`
	Model     string          `json:"model"`
}

type OrderSource struct {
	ChatType    string `json:"chat_type"` // "private" or "group"
	ChatID      string `json:"chat_id"`
	ChatName    string `json:"chat_name,omitempty"`
	SenderPhone string `json:"sender_phone"`
	SenderName  string `json:"sender_name"`
}

type SourceMessage struct {
	ID   string    `json:"id"`
	Time time.Time `json:"time"`
	Text string    `json:"text"`
}

// Deliverer sends finished orders to your system as a webhook: an HTTP POST with the
// order as JSON, signed with your secret. When your system doesn't answer with a 2xx
// status, the order is retried with growing pauses, at most an hour apart.
type Deliverer struct {
	url, secret string
	store       *Store
	client      *http.Client
	wake        chan struct{}

	logPath string
	logMu   sync.Mutex
}

func NewDeliverer(out OutputConfig, store *Store, logPath string) *Deliverer {
	return &Deliverer{
		url:     out.WebhookURL,
		secret:  out.Secret,
		store:   store,
		client:  &http.Client{Timeout: 30 * time.Second},
		wake:    make(chan struct{}, 1),
		logPath: logPath,
	}
}

// Enqueue saves the order and sends it as soon as possible. Every order is also
// appended to the local log file, one JSON object per line. Without a webhook URL
// (test mode) the log file is the only place it goes.
func (d *Deliverer) Enqueue(ctx context.Context, ev OrderEvent) error {
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if err := d.appendLog(body); err != nil {
		log.Printf("could not write %s: %v", d.logPath, err)
	}
	testMode := d.url == ""
	if err := d.store.SaveOrder(ctx, ev.ID, body, testMode); err != nil {
		return err
	}
	if !testMode {
		select {
		case d.wake <- struct{}{}:
		default:
		}
	}
	return nil
}

// Run sends waiting orders until ctx is cancelled.
func (d *Deliverer) Run(ctx context.Context) {
	if d.url == "" {
		return
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		d.deliverDue(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-d.wake:
		}
	}
}

func (d *Deliverer) deliverDue(ctx context.Context, now time.Time) {
	orders, err := d.store.DueOrders(ctx, now)
	if err != nil {
		log.Printf("could not load orders to send: %v", err)
		return
	}
	for _, o := range orders {
		if err := d.post(ctx, o.ID, o.Payload); err != nil {
			attempts := o.Attempts + 1
			retryIn := min(time.Minute<<min(attempts-1, 6), time.Hour)
			log.Printf("could not send order %s to your system, trying again in %s: %v", o.ID, retryIn, err)
			if err := d.store.MarkAttemptFailed(ctx, o.ID, attempts, now.Add(retryIn), err.Error()); err != nil {
				log.Printf("could not save the retry for order %s: %v", o.ID, err)
			}
			continue
		}
		if err := d.store.MarkDelivered(ctx, o.ID); err != nil {
			log.Printf("could not mark order %s as sent: %v", o.ID, err)
			continue
		}
		log.Printf("sent order %s to your system", o.ID)
	}
}

func (d *Deliverer) post(ctx context.Context, id string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "whatsapp-orders")
	req.Header.Set("X-Order-Id", id)
	req.Header.Set("X-Signature-256", "sha256="+sign(d.secret, body))
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("your system answered %s", resp.Status)
	}
	return nil
}

// sign is the HMAC-SHA256 of the body with your secret, as lowercase hex.
func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func (d *Deliverer) appendLog(line []byte) error {
	d.logMu.Lock()
	defer d.logMu.Unlock()
	f, err := os.OpenFile(d.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(append([]byte{}, line...), '\n'))
	return err
}
