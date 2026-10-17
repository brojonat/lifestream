package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Paid notifications (VISION.md "Paid notifications"): subscribers follow a
// topic, and the creator sends notifications on it by hand. A topic's id is
// the id of its topic.created event; a subscriber is an email address.
const (
	TypeTopicCreated      = "topic.created"
	TypeTopicAmended      = "topic.amended"
	TypeTopicSubscribed   = "topic.subscribed"
	TypeTopicUnsubscribed = "topic.unsubscribed"
	TypeNotificationSent  = "notification.sent"
)

// Price kinds: pay once and follow forever, or monthly.
const (
	PriceOneTime = "one_time"
	PriceMonthly = "monthly"
)

type (
	Price struct {
		Kind        string `json:"kind"`
		AmountCents int64  `json:"amount_cents"`
		Currency    string `json:"currency,omitempty"` // default "usd"
	}
	TopicCreated struct {
		Name         string     `json:"name"`
		Description  string     `json:"description,omitempty"`
		Price        Price      `json:"price"`
		Cap          *int       `json:"cap,omitempty"`           // sell to the first N only; nil is unlimited
		HistoryStart *time.Time `json:"history_start,omitempty"` // nil: each subscriber's own start
	}
	// TopicAmended changes what it sets. ClearCap and ClearHistoryStart go
	// back to unlimited and to "from subscription".
	TopicAmended struct {
		TopicID           string     `json:"topic_id"`
		Name              string     `json:"name,omitempty"`
		Description       *string    `json:"description,omitempty"`
		Price             *Price     `json:"price,omitempty"`
		Cap               *int       `json:"cap,omitempty"`
		ClearCap          bool       `json:"clear_cap,omitempty"`
		HistoryStart      *time.Time `json:"history_start,omitempty"`
		ClearHistoryStart bool       `json:"clear_history_start,omitempty"`
	}
	TopicSubscribed struct {
		TopicID    string `json:"topic_id"`
		Subscriber string `json:"subscriber"` // email
	}
	TopicUnsubscribed struct {
		TopicID    string `json:"topic_id"`
		Subscriber string `json:"subscriber"`
		Reason     string `json:"reason,omitempty"` // canceled, refunded, lapsed, ...
	}
	NotificationSent struct {
		TopicID string          `json:"topic_id"`
		Payload json.RawMessage `json:"payload"` // text, a link, JSON: rendered escaped, never as HTML
	}
)

// Topic is a topic's current definition.
type Topic struct {
	ID           string     `json:"id"`
	Owner        string     `json:"owner"`
	Name         string     `json:"name"`
	Description  string     `json:"description,omitempty"`
	Price        Price      `json:"price"`
	Cap          *int       `json:"cap,omitempty"`
	HistoryStart *time.Time `json:"history_start,omitempty"`
	Subscribers  int        `json:"subscribers"` // current
	CreatedAt    time.Time  `json:"created_at"`
}

// Notification is one sent notification as a subscriber sees it.
type Notification struct {
	EventID string          `json:"event_id"`
	TopicID string          `json:"topic_id"`
	Topic   string          `json:"topic"`
	SentAt  time.Time       `json:"sent_at"`
	Payload json.RawMessage `json:"payload"`
}

func normalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func checkPrice(p Price) (Price, error) {
	if p.Kind != PriceOneTime && p.Kind != PriceMonthly {
		return p, invalid("price kind must be %s or %s, got %q", PriceOneTime, PriceMonthly, p.Kind)
	}
	if p.AmountCents < 0 {
		return p, invalid("price can't be negative")
	}
	if p.Currency == "" {
		p.Currency = "usd"
	}
	p.Currency = strings.ToLower(p.Currency)
	return p, nil
}

func checkCap(c *int) error {
	if c != nil && *c < 1 {
		return invalid("cap must be at least 1 (leave it out for unlimited)")
	}
	return nil
}

// applyTopic projects the topic events; apply calls it for their types.
func applyTopic(ctx context.Context, tx *sql.Tx, e Event) error {
	switch e.Type {
	case TypeTopicCreated:
		var p TopicCreated
		if err := decode(e, &p); err != nil {
			return err
		}
		if strings.TrimSpace(p.Name) == "" {
			return invalid("%s needs a name", e.Type)
		}
		price, err := checkPrice(p.Price)
		if err != nil {
			return err
		}
		if err := checkCap(p.Cap); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO topics (id, owner, name, description, price_kind, price_cents, currency, cap, history_start, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, e.ID, e.Owner, strings.TrimSpace(p.Name), p.Description, price.Kind, price.AmountCents,
			price.Currency, p.Cap, timeOrNil(p.HistoryStart), fmtTime(e.OccurredAt))
		if err != nil {
			return fmt.Errorf("projecting %s: %w", e.Type, err)
		}

	case TypeTopicAmended:
		var p TopicAmended
		if err := decode(e, &p); err != nil {
			return err
		}
		if _, err := topicTx(ctx, tx, e.Owner, p.TopicID); err != nil {
			return err
		}
		if p.Cap != nil && p.ClearCap || p.HistoryStart != nil && p.ClearHistoryStart {
			return invalid("%s can't both set and clear the same field", e.Type)
		}
		sets, args := []string{}, []any{}
		set := func(col string, v any) { sets, args = append(sets, col+" = ?"), append(args, v) }
		if name := strings.TrimSpace(p.Name); name != "" {
			set("name", name)
		}
		if p.Description != nil {
			set("description", *p.Description)
		}
		if p.Price != nil {
			price, err := checkPrice(*p.Price)
			if err != nil {
				return err
			}
			set("price_kind", price.Kind)
			set("price_cents", price.AmountCents)
			set("currency", price.Currency)
		}
		if err := checkCap(p.Cap); err != nil {
			return err
		}
		if p.Cap != nil {
			set("cap", *p.Cap)
		}
		if p.ClearCap {
			set("cap", nil)
		}
		if p.HistoryStart != nil {
			set("history_start", fmtTime(*p.HistoryStart))
		}
		if p.ClearHistoryStart {
			set("history_start", nil)
		}
		if len(sets) == 0 {
			return invalid("%s changes nothing", e.Type)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE topics SET `+strings.Join(sets, ", ")+` WHERE id = ?`, append(args, p.TopicID)...); err != nil {
			return fmt.Errorf("projecting %s: %w", e.Type, err)
		}

	case TypeTopicSubscribed:
		var p TopicSubscribed
		if err := decode(e, &p); err != nil {
			return err
		}
		if _, err := topicTx(ctx, tx, e.Owner, p.TopicID); err != nil {
			return err
		}
		who := normalizeEmail(p.Subscriber)
		if !strings.Contains(who, "@") {
			return invalid("%s needs the subscriber's email", e.Type)
		}
		if active, err := activePeriod(ctx, tx, p.TopicID, who); err != nil {
			return err
		} else if active {
			return invalid("%s is already subscribed to %s", who, p.TopicID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO subscriptions (topic_id, subscriber, started_at, start_event_id) VALUES (?, ?, ?, ?)`,
			p.TopicID, who, fmtTime(e.OccurredAt), e.ID); err != nil {
			return fmt.Errorf("projecting %s: %w", e.Type, err)
		}

	case TypeTopicUnsubscribed:
		var p TopicUnsubscribed
		if err := decode(e, &p); err != nil {
			return err
		}
		if _, err := topicTx(ctx, tx, e.Owner, p.TopicID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE subscriptions SET ended_at = ?, end_reason = ? WHERE topic_id = ? AND subscriber = ? AND ended_at IS NULL`,
			fmtTime(e.OccurredAt), p.Reason, p.TopicID, normalizeEmail(p.Subscriber))
		if err != nil {
			return fmt.Errorf("projecting %s: %w", e.Type, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return invalid("%s is not subscribed to %s", p.Subscriber, p.TopicID)
		}

	case TypeNotificationSent:
		// The agent never sends: a misread text must not ping paying people.
		if e.Source == SourceAgent {
			return invalid("%s is sent by the creator, never by the agent", e.Type)
		}
		var p NotificationSent
		if err := decode(e, &p); err != nil {
			return err
		}
		if _, err := topicTx(ctx, tx, e.Owner, p.TopicID); err != nil {
			return err
		}
		if len(p.Payload) == 0 || string(p.Payload) == "null" || string(p.Payload) == `""` {
			return invalid("%s needs a payload", e.Type)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notifications (event_id, owner, topic_id, sent_at) VALUES (?, ?, ?, ?)`,
			e.ID, e.Owner, p.TopicID, fmtTime(e.OccurredAt)); err != nil {
			return fmt.Errorf("projecting %s: %w", e.Type, err)
		}
	}
	return nil
}

func timeOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return fmtTime(*t)
}

func activePeriod(ctx context.Context, tx *sql.Tx, topicID, subscriber string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx, `SELECT count(*) FROM subscriptions WHERE topic_id = ? AND subscriber = ? AND ended_at IS NULL`, topicID, subscriber).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("reading subscription: %w", err)
	}
	return n > 0, nil
}

const selectTopic = `SELECT t.id, t.owner, t.name, t.description, t.price_kind, t.price_cents, t.currency, t.cap, t.history_start, t.created_at,
	(SELECT count(*) FROM subscriptions s WHERE s.topic_id = t.id AND s.ended_at IS NULL) FROM topics t`

func scanTopic(row scanner) (Topic, error) {
	var t Topic
	var cap sql.NullInt64
	var history sql.NullString
	var created string
	err := row.Scan(&t.ID, &t.Owner, &t.Name, &t.Description, &t.Price.Kind, &t.Price.AmountCents, &t.Price.Currency, &cap, &history, &created, &t.Subscribers)
	if errors.Is(err, sql.ErrNoRows) {
		return Topic{}, ErrNotFound
	}
	if err != nil {
		return Topic{}, fmt.Errorf("reading topic: %w", err)
	}
	if cap.Valid {
		n := int(cap.Int64)
		t.Cap = &n
	}
	if history.Valid {
		h, err := parseTime(history.String)
		if err != nil {
			return Topic{}, err
		}
		t.HistoryStart = &h
	}
	t.CreatedAt, err = parseTime(created)
	return t, err
}

func topicTx(ctx context.Context, tx *sql.Tx, owner, id string) (Topic, error) {
	t, err := scanTopic(tx.QueryRowContext(ctx, selectTopic+` WHERE t.id = ?`, id))
	if errors.Is(err, ErrNotFound) || (err == nil && t.Owner != owner) {
		return Topic{}, invalid("no topic %q for this owner", id)
	}
	return t, err
}

// Topics returns an owner's topics, oldest first, with subscriber counts.
func (s *Store) Topics(ctx context.Context, owner string) ([]Topic, error) {
	rows, err := s.db.QueryContext(ctx, selectTopic+` WHERE t.owner = ? ORDER BY t.created_at, t.id`, owner)
	if err != nil {
		return nil, fmt.Errorf("listing topics: %w", err)
	}
	defer rows.Close()
	out := []Topic{}
	for rows.Next() {
		t, err := scanTopic(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Topic returns one topic.
func (s *Store) Topic(ctx context.Context, id string) (Topic, error) {
	return scanTopic(s.db.QueryRowContext(ctx, selectTopic+` WHERE t.id = ?`, id))
}

// SubscriberInbox returns what a subscriber can read, newest first: every
// notification on a topic they followed, sent during one of their
// subscription periods, or before it but after the topic's history start.
// Computed on read, so sending is one event however many subscribe.
func (s *Store) SubscriberInbox(ctx context.Context, subscriber string, limit int) ([]Notification, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT n.event_id, n.topic_id, t.name, n.sent_at, e.payload
		FROM notifications n
		JOIN topics t ON t.id = n.topic_id
		JOIN subscriptions s ON s.topic_id = n.topic_id AND s.subscriber = ?
		JOIN events e ON e.id = n.event_id
		WHERE n.sent_at >= COALESCE(t.history_start, s.started_at)
		AND (s.ended_at IS NULL OR n.sent_at <= s.ended_at)
		ORDER BY n.sent_at DESC, n.event_id DESC LIMIT ?`, normalizeEmail(subscriber), limit)
	if err != nil {
		return nil, fmt.Errorf("reading subscriber inbox: %w", err)
	}
	defer rows.Close()
	out := []Notification{}
	for rows.Next() {
		var n Notification
		var at, payload string
		if err := rows.Scan(&n.EventID, &n.TopicID, &n.Topic, &at, &payload); err != nil {
			return nil, fmt.Errorf("reading subscriber inbox: %w", err)
		}
		if n.SentAt, err = parseTime(at); err != nil {
			return nil, err
		}
		var p NotificationSent
		json.Unmarshal([]byte(payload), &p)
		n.Payload = p.Payload
		out = append(out, n)
	}
	return out, rows.Err()
}
