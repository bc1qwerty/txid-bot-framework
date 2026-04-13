// Example: per-condition broadcast bot using composite Subscriptions.
//
// This is the reference implementation for the bangool/bid-alert pattern:
// one user can hold multiple "filter slots" under the same chat, each
// with its own ItemFilter result and per-slot dispatch tracking via
// bot_sent.
//
// Run:
//
//	BOT_TOKEN=... go run ./examples/conditional
//
// On startup it seeds two demo Subscriptions for chat "1001":
//
//	1001:cheap   - Meta{"max_price": "100"}
//	1001:premium - Meta{"min_price": "500"}
//
// Then the FakeSource emits three items priced 50, 200, 800. The runner
// dispatches:
//
//	item 50  → matches 1001:cheap  (price <= 100)
//	item 200 → matches neither
//	item 800 → matches 1001:premium (price >= 500)
//
// Both subscriptions share the same Recipient ("1001") so a real
// Telegram bot would send to the same chat from two slots. With
// SubscriberFormatter you can distinguish which slot triggered each
// alert in the rendered text.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/bc1qwerty/txid-bot-framework/pkg/bot"
	"github.com/bc1qwerty/txid-bot-framework/pkg/core"
	"github.com/bc1qwerty/txid-bot-framework/pkg/store"
)

// FakeSource emits a fixed slice of items on the first poll, then
// nothing on subsequent polls. Lets the example run without an
// external API dependency.
type FakeSource struct {
	items     []core.Item
	delivered bool
}

func (f *FakeSource) Name() string { return "fake" }

func (f *FakeSource) Fetch(ctx context.Context) ([]core.Item, error) {
	if f.delivered {
		return nil, nil
	}
	f.delivered = true
	return f.items, nil
}

// PerSubFormatter implements core.Formatter AND the optional
// bot.SubscriberFormatter. The runner detects the latter and calls
// FormatFor per subscription, so the rendered text can include the
// matching slot ID.
type PerSubFormatter struct{}

func (PerSubFormatter) Format(item core.Item) core.Message {
	// Fallback used only if the runner's SubscriberFormatter detection
	// fails (which it shouldn't since we implement both methods on the
	// same type).
	return core.Message{Text: "item " + item.ID + " priced " + item.Meta["price"]}
}

func (PerSubFormatter) FormatFor(sub store.Subscription, item core.Item) core.Message {
	slot := sub.Meta["slot"]
	return core.Message{
		Text: fmt.Sprintf("[slot %s] item %s priced %s",
			slot, item.ID, item.Meta["price"]),
	}
}

// LoggingNotifier prints every Send call instead of talking to a real
// channel. Replace with notify.Telegram in a production bot.
type LoggingNotifier struct{}

func (LoggingNotifier) Name() string { return "log" }

func (LoggingNotifier) Send(ctx context.Context, recipient string, msg core.Message) error {
	log.Printf("DELIVER recipient=%s text=%q", recipient, msg.Text)
	return nil
}

// matchesPriceRange reads min/max price from sub.Meta and compares the
// item's price field. Missing constraints mean "no limit" (legacy
// semantics).
func matchesPriceRange(sub store.Subscription, item core.Item) bool {
	itemPrice, err := strconv.Atoi(item.Meta["price"])
	if err != nil {
		// No price → never reject (item has no price to compare against).
		return true
	}
	if minStr := sub.Meta["min_price"]; minStr != "" {
		if minP, err := strconv.Atoi(minStr); err == nil && itemPrice < minP {
			return false
		}
	}
	if maxStr := sub.Meta["max_price"]; maxStr != "" {
		if maxP, err := strconv.Atoi(maxStr); err == nil && itemPrice > maxP {
			return false
		}
	}
	return true
}

func main() {
	// In-memory sqlite so the example is self-contained.
	st, err := store.Open(":memory:", "conditional-example")
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	// Seed two slots for chat "1001". A real bot would call
	// SubscribeRich from a /add command handler, persisting whatever
	// filter state the user provided.
	must(st.SubscribeRich(store.Subscription{
		ID:        "1001:cheap",
		Recipient: "1001",
		Meta: map[string]string{
			"slot":      "cheap",
			"max_price": "100",
		},
	}))
	must(st.SubscribeRich(store.Subscription{
		ID:        "1001:premium",
		Recipient: "1001",
		Meta: map[string]string{
			"slot":      "premium",
			"min_price": "500",
		},
	}))

	src := &FakeSource{
		items: []core.Item{
			{ID: "tx-50", Title: "cheap deal", Meta: map[string]string{"price": "50"}},
			{ID: "tx-200", Title: "mid deal", Meta: map[string]string{"price": "200"}},
			{ID: "tx-800", Title: "premium deal", Meta: map[string]string{"price": "800"}},
		},
	}

	r := bot.New(bot.Config{
		Name:         "conditional-example",
		Source:       src,
		Formatter:    PerSubFormatter{},
		Notifier:     LoggingNotifier{},
		Store:        st,
		PollInterval: time.Hour,
		ItemFilter:   func(_ context.Context, sub store.Subscription, item core.Item) bool { return matchesPriceRange(sub, item) },
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		// Run for one poll cycle then stop.
		time.Sleep(2 * time.Second)
		cancel()
	}()
	go func() {
		<-sig
		cancel()
	}()

	if err := r.Run(ctx); err != nil && err != context.Canceled {
		log.Fatalf("runner: %v", err)
	}
	log.Println("example complete")
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
