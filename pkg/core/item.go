// Package core defines the common types and interfaces for txid bots.
package core

import (
	"context"
	"time"
)

// Item is a generic unit fetched from a data source.
type Item struct {
	ID        string // unique identifier for deduplication
	Title     string
	Content   string
	URL       string
	Category  string
	ImageURL  string
	// ImageData carries an image the Source already holds in memory, for
	// sources that inline it (e.g. a base64 data: URI in a JSON payload)
	// and therefore have no URL Telegram could fetch on its own.
	// json:"-" keeps it out of the archiver's JSONL: a megabyte of
	// base64 per item would balloon the daily backup.
	ImageData []byte `json:"-"`
	// ImageName is the filename sent alongside ImageData. Telegram uses
	// its extension to pick a MIME type, so it must match the actual
	// bytes (e.g. "alert.png"). Empty falls back to "image.jpg".
	ImageName string
	Timestamp time.Time
	Meta      map[string]string
}

// Message is the output of formatting an Item for delivery.
type Message struct {
	Text      string
	ParseMode string // "Markdown", "HTML", or ""
	// PlainText is an optional plain-text variant used by channels that
	// cannot parse Text's ParseMode (e.g., Naver Band, raw webhooks).
	// When empty, channels fall back to Text.
	PlainText string
	ImageURL  string
	// ImageData is an in-memory image to upload. It takes precedence over
	// ImageURL. Channels that cannot post binary (Naver Band) ignore it.
	ImageData []byte
	ImageName string
	Buttons   [][]Button
}

// Button is an inline keyboard button.
type Button struct {
	Text string
	URL  string
	Data string // callback data
}

// Source fetches items from an external data source.
type Source interface {
	Name() string
	Fetch(ctx context.Context) ([]Item, error)
}

// Formatter converts an Item into a deliverable Message.
type Formatter interface {
	Format(item Item) Message
}

// Notifier delivers a Message to a recipient.
type Notifier interface {
	Name() string
	Send(ctx context.Context, recipient string, msg Message) error
}
