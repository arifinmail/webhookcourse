package main

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Store keeps the messages that passed the filter and the orders made from them, so
// nothing is lost if the program restarts or your system is down for a while.
type Store struct{ db *sql.DB }

const storeSchema = `
CREATE TABLE IF NOT EXISTS messages (
	chat_id      TEXT NOT NULL,
	id           TEXT NOT NULL,
	chat_name    TEXT NOT NULL,
	is_group     INTEGER NOT NULL,
	sender_id    TEXT NOT NULL,
	sender_phone TEXT NOT NULL,
	sender_name  TEXT NOT NULL,
	sent_at      INTEGER NOT NULL,
	text         TEXT NOT NULL,
	-- pending, ordered, not_order, skipped or error
	status       TEXT NOT NULL DEFAULT 'pending',
	order_id     TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (chat_id, id)
);
CREATE TABLE IF NOT EXISTS orders (
	id              TEXT PRIMARY KEY,
	created_at      INTEGER NOT NULL,
	payload         TEXT NOT NULL,
	delivered       INTEGER NOT NULL DEFAULT 0,
	attempts        INTEGER NOT NULL DEFAULT 0,
	next_attempt_at INTEGER NOT NULL DEFAULT 0,
	last_error      TEXT NOT NULL DEFAULT ''
);`

func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(storeSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// SaveMessage stores a message and reports whether it was new; WhatsApp sometimes
// delivers the same message twice.
func (s *Store) SaveMessage(ctx context.Context, m Incoming) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO messages
		(chat_id, id, chat_name, is_group, sender_id, sender_phone, sender_name, sent_at, text)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ChatID, m.ID, m.ChatName, m.IsGroup, m.SenderID, m.SenderPhone, m.SenderName, m.Time.UnixMilli(), m.Text)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// PendingMessages returns the messages that were still waiting to be read when the
// program last stopped.
func (s *Store) PendingMessages(ctx context.Context) ([]Incoming, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT chat_id, id, chat_name, is_group, sender_id, sender_phone, sender_name, sent_at, text
		FROM messages WHERE status = 'pending' ORDER BY sent_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Incoming
	for rows.Next() {
		var m Incoming
		var sentAt int64
		if err := rows.Scan(&m.ChatID, &m.ID, &m.ChatName, &m.IsGroup, &m.SenderID, &m.SenderPhone, &m.SenderName, &sentAt, &m.Text); err != nil {
			return nil, err
		}
		m.Time = time.UnixMilli(sentAt)
		out = append(out, m)
	}
	return out, rows.Err()
}

// MarkMessages records what happened to a batch.
func (s *Store) MarkMessages(ctx context.Context, batch []Incoming, status, orderID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, m := range batch {
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET status = ?, order_id = ? WHERE chat_id = ? AND id = ?`,
			status, orderID, m.ChatID, m.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type storedOrder struct {
	ID       string
	Payload  []byte
	Attempts int
}

func (s *Store) SaveOrder(ctx context.Context, id string, payload []byte, delivered bool) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO orders (id, created_at, payload, delivered) VALUES (?, ?, ?, ?)`,
		id, time.Now().UnixMilli(), string(payload), delivered)
	return err
}

// DueOrders returns orders your system hasn't accepted yet whose next attempt is due.
func (s *Store) DueOrders(ctx context.Context, now time.Time) ([]storedOrder, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, payload, attempts FROM orders
		WHERE delivered = 0 AND next_attempt_at <= ? ORDER BY created_at LIMIT 50`, now.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storedOrder
	for rows.Next() {
		var o storedOrder
		var payload string
		if err := rows.Scan(&o.ID, &payload, &o.Attempts); err != nil {
			return nil, err
		}
		o.Payload = []byte(payload)
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Store) MarkDelivered(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE orders SET delivered = 1, last_error = '' WHERE id = ?`, id)
	return err
}

func (s *Store) MarkAttemptFailed(ctx context.Context, id string, attempts int, next time.Time, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE orders SET attempts = ?, next_attempt_at = ?, last_error = ? WHERE id = ?`,
		attempts, next.UnixMilli(), reason, id)
	return err
}
