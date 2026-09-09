// Package store provides a SQLite-backed state store for bots.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Subscription is a single dispatch slot. A user may have multiple
// subscriptions under one bot_key when each subscription carries its
// own filter state (per-condition bots like bangool/bid-alert).
//
//	ID        - opaque, unique within a bot_key. For simple one-per-user
//	            bots this is just the chat_id as a string.
//	Recipient - the actual endpoint (e.g., Telegram chat_id as a string).
//	            Multiple Subscriptions can share a Recipient.
//	Meta      - arbitrary filter/format state stored as JSON. The framework
//	            only persists it verbatim; interpretation is the bot's job.
type Subscription struct {
	ID        string
	Recipient string
	Meta      map[string]string
}

// Store handles subscribers, sent alerts, and custom bot state.
type Store struct {
	db     *sql.DB
	botKey string // namespace for this bot (e.g., "food-recall")
}

// Open opens or creates a SQLite database at the given path.
// botKey namespaces the tables so multiple bots can share a file.
func Open(dbPath, botKey string) (*Store, error) {
	dir := filepath.Dir(dbPath)
	_ = dir // reserved for mkdir if needed

	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}

	s := &Store{db: db, botKey: botKey}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

// DB returns the raw database handle for bot-specific tables.
func (s *Store) DB() *sql.DB {
	return s.db
}

// BotKey returns the namespace identifier for this bot instance.
func (s *Store) BotKey() string {
	return s.botKey
}

func (s *Store) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS bot_subscribers (
		bot_key TEXT NOT NULL,
		chat_id TEXT NOT NULL,
		subscribed_at INTEGER NOT NULL DEFAULT (unixepoch()),
		active INTEGER NOT NULL DEFAULT 1,
		PRIMARY KEY (bot_key, chat_id)
	);

	CREATE TABLE IF NOT EXISTS bot_seen (
		bot_key TEXT NOT NULL,
		source TEXT NOT NULL,
		item_id TEXT NOT NULL,
		seen_at INTEGER NOT NULL DEFAULT (unixepoch()),
		PRIMARY KEY (bot_key, source, item_id)
	);
	CREATE INDEX IF NOT EXISTS idx_bot_seen_at ON bot_seen(seen_at);

	-- 발송 실패 카운터.
	-- ⚠ 예전에는 Notifier.Send 가 실패해도 아이템을 무조건 MarkSeen 해서, 다음 폴의
	--   IsSeen 게이트가 그 아이템을 걸러 **재시도 기회가 영영 없었다**. 텔레그램이
	--   429 를 잠깐 내기만 해도 그 알림은 영구 유실되고 흔적은 로그 한 줄뿐이었다.
	--   이제는 실패하면 seen 처리를 미뤄 다음 폴에서 다시 시도하되, 무한 재시도를
	--   막기 위해 시도 횟수를 여기 센다. 상한을 넘기면 포기하고 seen 처리한다.
	CREATE TABLE IF NOT EXISTS bot_send_failure (
		bot_key TEXT NOT NULL,
		source TEXT NOT NULL,
		item_id TEXT NOT NULL,
		attempts INTEGER NOT NULL DEFAULT 0,
		last_error TEXT,
		updated_at INTEGER NOT NULL DEFAULT (unixepoch()),
		PRIMARY KEY (bot_key, source, item_id)
	);

	CREATE TABLE IF NOT EXISTS bot_sent (
		bot_key TEXT NOT NULL,
		chat_id TEXT NOT NULL,
		item_id TEXT NOT NULL,
		sent_at INTEGER NOT NULL DEFAULT (unixepoch()),
		PRIMARY KEY (bot_key, chat_id, item_id)
	);
	CREATE INDEX IF NOT EXISTS idx_bot_sent_at ON bot_sent(sent_at);
	`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}

	// Lazy migration: add Subscription fields to bot_subscribers if they are
	// missing. Older stores created before the Subscription abstraction will
	// silently gain these columns on first open. An empty recipient column
	// means "use chat_id as the recipient" (backward compat).
	if err := s.addColumnIfMissing("bot_subscribers", "recipient", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("migrate recipient: %w", err)
	}
	if err := s.addColumnIfMissing("bot_subscribers", "meta", "TEXT NOT NULL DEFAULT '{}'"); err != nil {
		return fmt.Errorf("migrate meta: %w", err)
	}
	return nil
}

// addColumnIfMissing runs ALTER TABLE ADD COLUMN only when the column is
// absent from table_info. sqlite has no IF NOT EXISTS for ADD COLUMN.
func (s *Store) addColumnIfMissing(table, column, colDef string) error {
	rows, err := s.db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, colType string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil // already present
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, colDef))
	return err
}

// Subscribe adds a chat to this bot's subscribers. Equivalent to
// SubscribeRich with Subscription{ID: chatID, Recipient: chatID}.
func (s *Store) Subscribe(chatID string) error {
	return s.SubscribeRich(Subscription{ID: chatID, Recipient: chatID})
}

// SubscribeRich inserts or updates a Subscription, preserving the opaque
// ID, recipient, and meta fields. Use this when a user can hold multiple
// subscriptions with different filter state.
//
// For simple bots, prefer Subscribe(chatID) which delegates here with
// ID = Recipient = chatID and empty Meta.
func (s *Store) SubscribeRich(sub Subscription) error {
	if sub.ID == "" {
		return fmt.Errorf("subscription ID is required")
	}
	recipient := sub.Recipient
	if recipient == "" {
		recipient = sub.ID
	}
	metaJSON := "{}"
	if len(sub.Meta) > 0 {
		b, err := json.Marshal(sub.Meta)
		if err != nil {
			return fmt.Errorf("marshal meta: %w", err)
		}
		metaJSON = string(b)
	}
	_, err := s.db.Exec(
		`INSERT INTO bot_subscribers (bot_key, chat_id, recipient, meta, active) VALUES (?, ?, ?, ?, 1)
		 ON CONFLICT(bot_key, chat_id) DO UPDATE SET
		     recipient = excluded.recipient,
		     meta = excluded.meta,
		     active = 1,
		     subscribed_at = unixepoch()`,
		s.botKey, sub.ID, recipient, metaJSON)
	return err
}

// ActiveSubscriptions returns all active Subscriptions for this bot_key.
// Older rows with an empty recipient fall back to their chat_id (legacy
// behavior: the recipient IS the opaque ID).
func (s *Store) ActiveSubscriptions() ([]Subscription, error) {
	rows, err := s.db.Query(
		`SELECT chat_id, recipient, meta FROM bot_subscribers
		 WHERE bot_key = ? AND active = 1`,
		s.botKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Subscription
	for rows.Next() {
		var sub Subscription
		var recipient, metaJSON sql.NullString
		if err := rows.Scan(&sub.ID, &recipient, &metaJSON); err != nil {
			return nil, err
		}
		if recipient.Valid && recipient.String != "" {
			sub.Recipient = recipient.String
		} else {
			sub.Recipient = sub.ID
		}
		if metaJSON.Valid && metaJSON.String != "" && metaJSON.String != "{}" {
			m := make(map[string]string)
			if err := json.Unmarshal([]byte(metaJSON.String), &m); err == nil {
				sub.Meta = m
			}
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// Unsubscribe marks a chat as inactive.
func (s *Store) Unsubscribe(chatID string) error {
	_, err := s.db.Exec(
		`UPDATE bot_subscribers SET active = 0 WHERE bot_key = ? AND chat_id = ?`,
		s.botKey, chatID)
	return err
}

// DeactivateRecipient 는 **한 수신자에게 딸린 모든 구독**을 끄고 끈 개수를 준다.
//
// Unsubscribe 와 다른 점은 키다. Unsubscribe 는 chat_id(=Subscription.ID) 하나를
// 끄는데, 봇에 따라 한 사람이 조건별로 여러 슬롯을 가진다
// (nara-bot 의 `5385429383:1`·`:2` — chat_id 는 슬롯마다 다르고 recipient 만 같다).
// 사람이 봇을 **차단**했다면 그 사람의 슬롯이 전부 못 가므로 recipient 로 끊어야 한다.
//
// ⚠매칭식은 ActiveSubscriptions 의 폴백과 **정확히 같아야** 한다. 옛 행은 recipient
// 가 빈 문자열이고 그때는 chat_id 가 곧 수신자다 — 여기서 그 폴백을 빠뜨리면
// 옛 행은 영영 안 꺼지고 소음이 남는다.
func (s *Store) DeactivateRecipient(recipient string) (int, error) {
	res, err := s.db.Exec(
		`UPDATE bot_subscribers SET active = 0
		 WHERE bot_key = ? AND active = 1
		   AND COALESCE(NULLIF(recipient, ''), chat_id) = ?`,
		s.botKey, recipient)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ActiveSubscribers returns all currently-subscribed chat IDs.
func (s *Store) ActiveSubscribers() ([]string, error) {
	rows, err := s.db.Query(
		`SELECT chat_id FROM bot_subscribers WHERE bot_key = ? AND active = 1`,
		s.botKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// IsSeen returns true if the item was previously fetched from this source.
func (s *Store) IsSeen(source, itemID string) (bool, error) {
	var cnt int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM bot_seen WHERE bot_key = ? AND source = ? AND item_id = ?`,
		s.botKey, source, itemID).Scan(&cnt)
	return cnt > 0, err
}

// MarkSeen records that an item was fetched.
// RecordSendFailure 는 (source, itemID) 의 발송 실패 횟수를 1 올리고 누적값을 준다.
// 반환값이 상한에 도달하면 호출자는 재시도를 포기하고 seen 처리해야 한다 —
// 그러지 않으면 chat not found 같은 영구 실패가 매 폴마다 영원히 재시도된다.
func (s *Store) RecordSendFailure(source, itemID, errMsg string) (int, error) {
	if len(errMsg) > 500 {
		errMsg = errMsg[:500]
	}
	_, err := s.db.Exec(`
		INSERT INTO bot_send_failure (bot_key, source, item_id, attempts, last_error, updated_at)
		VALUES (?, ?, ?, 1, ?, unixepoch())
		ON CONFLICT(bot_key, source, item_id) DO UPDATE SET
			attempts = attempts + 1,
			last_error = excluded.last_error,
			updated_at = unixepoch()`,
		s.botKey, source, itemID, errMsg)
	if err != nil {
		return 0, err
	}
	var attempts int
	err = s.db.QueryRow(
		`SELECT attempts FROM bot_send_failure WHERE bot_key = ? AND source = ? AND item_id = ?`,
		s.botKey, source, itemID).Scan(&attempts)
	return attempts, err
}

// ClearSendFailure 는 아이템이 결국 성공(또는 포기)했을 때 카운터를 지운다.
func (s *Store) ClearSendFailure(source, itemID string) error {
	_, err := s.db.Exec(
		`DELETE FROM bot_send_failure WHERE bot_key = ? AND source = ? AND item_id = ?`,
		s.botKey, source, itemID)
	return err
}

func (s *Store) MarkSeen(source, itemID string) error {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO bot_seen (bot_key, source, item_id) VALUES (?, ?, ?)`,
		s.botKey, source, itemID)
	return err
}

// IsSent returns true if an alert was already delivered to the given chat.
func (s *Store) IsSent(chatID, itemID string) (bool, error) {
	var cnt int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM bot_sent WHERE bot_key = ? AND chat_id = ? AND item_id = ?`,
		s.botKey, chatID, itemID).Scan(&cnt)
	return cnt > 0, err
}

// MarkSent records that an alert was delivered.
func (s *Store) MarkSent(chatID, itemID string) error {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO bot_sent (bot_key, chat_id, item_id) VALUES (?, ?, ?)`,
		s.botKey, chatID, itemID)
	return err
}

// Cleanup removes records older than the given retention period.
func (s *Store) Cleanup(retain time.Duration) error {
	cutoff := time.Now().Add(-retain).Unix()
	if _, err := s.db.Exec(
		`DELETE FROM bot_seen WHERE bot_key = ? AND seen_at < ?`, s.botKey, cutoff); err != nil {
		return err
	}
	if _, err := s.db.Exec(
		`DELETE FROM bot_sent WHERE bot_key = ? AND sent_at < ?`, s.botKey, cutoff); err != nil {
		return err
	}
	return nil
}
