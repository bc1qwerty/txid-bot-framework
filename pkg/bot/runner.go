// Package bot wires together a Source, Formatter, Notifier, and Store
// into a runnable poller with graceful shutdown.
package bot

import (
	"context"
	"log"
	"time"

	"github.com/bc1qwerty/txid-bot-framework/pkg/core"
	"github.com/bc1qwerty/txid-bot-framework/pkg/store"
)

// Config carries everything needed to run a bot.
type Config struct {
	// Name identifies this bot for logs and state namespacing.
	Name string

	// Source fetches new items.
	Source core.Source

	// Formatter converts Items to Messages.
	Formatter core.Formatter

	// Notifier delivers messages to recipients.
	Notifier core.Notifier

	// Store persists subscribers + dedup state.
	Store *store.Store

	// PollInterval is how often Fetch is called.
	PollInterval time.Duration

	// InitialDelay before the first poll (optional).
	InitialDelay time.Duration

	// RetainDuration is how long to keep seen/sent records.
	// If zero, defaults to 90 days.
	RetainDuration time.Duration

	// OnError is called when polling hits an error (optional).
	// Useful for sending admin notifications.
	OnError func(err error)

	// OnNewItem is called for each newly-fetched item before dispatch.
	// Runs after dedup filtering, once per item regardless of subscriber count.
	// Useful for fan-out to external channels (notification hub, logs).
	// A non-nil return is logged, not fatal - dispatch still proceeds.
	OnNewItem func(ctx context.Context, item core.Item) error

	// ItemFilter decides whether a given subscriber should receive a given
	// item. Return true to deliver, false to skip.
	//
	// When nil, all items are delivered to all active subscribers (default
	// broadcast behavior). When non-nil, filtered-out (item, chatID) pairs
	// are NOT marked as sent, so a later filter change can deliver them.
	//
	// Performance note: this runs O(items × subscribers) per poll. Load any
	// per-subscriber filter state into a closure at poll start rather than
	// querying inside the filter body. Example:
	//
	//	Config{ ItemFilter: func(ctx context.Context, chatID string, it core.Item) bool {
	//		// closure over pre-loaded map[chatID]filterCond
	//		return matches(conds[chatID], it)
	//	}}
	ItemFilter func(ctx context.Context, chatID string, item core.Item) bool
}

// Runner executes the poll loop.
type Runner struct {
	cfg Config
	log *log.Logger
}

// New creates a Runner from config.
func New(cfg Config) *Runner {
	return &Runner{
		cfg: cfg,
		log: log.New(log.Writer(), "["+cfg.Name+"] ", log.LstdFlags),
	}
}

// Run starts the poll loop and blocks until ctx is cancelled.
func (r *Runner) Run(ctx context.Context) error {
	r.log.Printf("runner started: interval=%s", r.cfg.PollInterval)

	if r.cfg.InitialDelay > 0 {
		select {
		case <-time.After(r.cfg.InitialDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Initial poll
	r.pollOnce(ctx)

	// Start cleanup goroutine
	go r.runCleanup(ctx)

	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			r.log.Println("runner stopped")
			return ctx.Err()
		case <-ticker.C:
			r.pollOnce(ctx)
		}
	}
}

// pollOnce runs a single fetch-filter-notify cycle.
func (r *Runner) pollOnce(ctx context.Context) {
	source := r.cfg.Source.Name()
	r.log.Printf("polling source=%s", source)

	items, err := r.cfg.Source.Fetch(ctx)
	if err != nil {
		r.log.Printf("fetch error: %v", err)
		if r.cfg.OnError != nil {
			r.cfg.OnError(err)
		}
		return
	}

	if len(items) == 0 {
		r.log.Printf("no items fetched")
		return
	}

	// Filter out items we've already seen
	newItems := make([]core.Item, 0, len(items))
	for _, item := range items {
		seen, err := r.cfg.Store.IsSeen(source, item.ID)
		if err != nil {
			r.log.Printf("seen check error: %v", err)
			continue
		}
		if !seen {
			newItems = append(newItems, item)
		}
	}

	if len(newItems) == 0 {
		r.log.Printf("no new items")
		return
	}

	// Get subscribers
	subs, err := r.cfg.Store.ActiveSubscribers()
	if err != nil {
		r.log.Printf("subscribers error: %v", err)
		return
	}

	r.log.Printf("dispatching %d new items to %d subscribers", len(newItems), len(subs))

	// Notify
	for _, item := range newItems {
		if r.cfg.OnNewItem != nil {
			if err := r.cfg.OnNewItem(ctx, item); err != nil {
				r.log.Printf("OnNewItem hook error (item=%s): %v", item.ID, err)
			}
		}
		msg := r.cfg.Formatter.Format(item)

		for _, chatID := range subs {
			sent, err := r.cfg.Store.IsSent(chatID, item.ID)
			if err != nil || sent {
				continue
			}
			if r.cfg.ItemFilter != nil && !r.cfg.ItemFilter(ctx, chatID, item) {
				// Intentionally NOT marking sent: if the filter condition
				// changes before the item ages out of bot_seen, we want the
				// next poll to re-evaluate and potentially deliver it.
				continue
			}
			if err := r.cfg.Notifier.Send(ctx, chatID, msg); err != nil {
				r.log.Printf("send error chat=%s: %v", chatID, err)
				continue
			}
			if err := r.cfg.Store.MarkSent(chatID, item.ID); err != nil {
				r.log.Printf("mark sent error: %v", err)
			}
		}

		if err := r.cfg.Store.MarkSeen(source, item.ID); err != nil {
			r.log.Printf("mark seen error: %v", err)
		}
	}
}

// runCleanup periodically purges old records.
func (r *Runner) runCleanup(ctx context.Context) {
	retain := r.cfg.RetainDuration
	if retain == 0 {
		retain = 90 * 24 * time.Hour
	}

	// Run daily
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.cfg.Store.Cleanup(retain); err != nil {
				r.log.Printf("cleanup error: %v", err)
			} else {
				r.log.Printf("cleanup done (retain=%s)", retain)
			}
		}
	}
}
