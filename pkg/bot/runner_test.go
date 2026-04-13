package bot

import (
	"context"
	"testing"
	"time"

	"github.com/bc1qwerty/txid-bot-framework/pkg/core"
	"github.com/bc1qwerty/txid-bot-framework/pkg/store"
)

type fakeSource struct {
	items []core.Item
}

func (f *fakeSource) Name() string { return "fake" }
func (f *fakeSource) Fetch(ctx context.Context) ([]core.Item, error) {
	return f.items, nil
}

type fakeFormatter struct{}

func (fakeFormatter) Format(it core.Item) core.Message {
	return core.Message{Text: it.Title}
}

type fakeNotifier struct {
	sent []string
}

func (f *fakeNotifier) Name() string { return "fake" }
func (f *fakeNotifier) Send(ctx context.Context, recipient string, msg core.Message) error {
	f.sent = append(f.sent, recipient+":"+msg.Text)
	return nil
}

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(":memory:", "test-bot")
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestOnNewItemHook(t *testing.T) {
	st := newTestStore(t)
	_ = st.Subscribe("100")
	_ = st.Subscribe("200")

	src := &fakeSource{items: []core.Item{{ID: "a", Title: "Alpha"}, {ID: "b", Title: "Bravo"}}}
	notifier := &fakeNotifier{}

	var hookCalls []string
	r := New(Config{
		Name: "test", Source: src, Formatter: fakeFormatter{},
		Notifier: notifier, Store: st, PollInterval: time.Hour,
		OnNewItem: func(ctx context.Context, item core.Item) error {
			hookCalls = append(hookCalls, item.ID)
			return nil
		},
	})
	r.pollOnce(context.Background())
	if len(hookCalls) != 2 {
		t.Fatalf("expected 2 OnNewItem calls, got %d", len(hookCalls))
	}
}

// TestItemFilterNil verifies backward compatibility: nil filter means
// every active subscriber receives every new item.
func TestItemFilterNil(t *testing.T) {
	st := newTestStore(t)
	_ = st.Subscribe("1")
	_ = st.Subscribe("2")

	src := &fakeSource{items: []core.Item{{ID: "x", Title: "X"}}}
	notifier := &fakeNotifier{}

	r := New(Config{
		Name: "test", Source: src, Formatter: fakeFormatter{},
		Notifier: notifier, Store: st, PollInterval: time.Hour,
		// ItemFilter: nil (default)
	})
	r.pollOnce(context.Background())

	if len(notifier.sent) != 2 {
		t.Errorf("expected 2 sends (broadcast), got %d", len(notifier.sent))
	}
}

// TestItemFilterRejectAll verifies that a filter returning false prevents
// sends AND does not mark the subscriber as sent (so later filter changes
// could still deliver within the same bot_seen lifetime).
func TestItemFilterRejectAll(t *testing.T) {
	st := newTestStore(t)
	_ = st.Subscribe("1")
	_ = st.Subscribe("2")

	src := &fakeSource{items: []core.Item{{ID: "x", Title: "X"}}}
	notifier := &fakeNotifier{}

	r := New(Config{
		Name: "test", Source: src, Formatter: fakeFormatter{},
		Notifier: notifier, Store: st, PollInterval: time.Hour,
		ItemFilter: func(ctx context.Context, chatID string, it core.Item) bool {
			return false
		},
	})
	r.pollOnce(context.Background())

	if len(notifier.sent) != 0 {
		t.Errorf("expected 0 sends (all filtered), got %d", len(notifier.sent))
	}

	// No MarkSent calls should have happened. Verify by checking IsSent.
	for _, chatID := range []string{"1", "2"} {
		sent, _ := st.IsSent(chatID, "x")
		if sent {
			t.Errorf("chat %s was marked sent despite filter reject", chatID)
		}
	}

	// bot_seen should still track the item (source-level dedup survives).
	seen, _ := st.IsSeen(src.Name(), "x")
	if !seen {
		t.Errorf("item should be in bot_seen even when fully filtered")
	}
}

// TestItemFilterSelective verifies per-subscriber filter routing:
// only chatID "allow" receives; "deny" is skipped and unmarked.
func TestItemFilterSelective(t *testing.T) {
	st := newTestStore(t)
	_ = st.Subscribe("allow")
	_ = st.Subscribe("deny")

	src := &fakeSource{items: []core.Item{{ID: "x", Title: "X"}}}
	notifier := &fakeNotifier{}

	r := New(Config{
		Name: "test", Source: src, Formatter: fakeFormatter{},
		Notifier: notifier, Store: st, PollInterval: time.Hour,
		ItemFilter: func(ctx context.Context, chatID string, it core.Item) bool {
			return chatID == "allow"
		},
	})
	r.pollOnce(context.Background())

	if len(notifier.sent) != 1 {
		t.Fatalf("expected 1 send, got %d: %v", len(notifier.sent), notifier.sent)
	}
	if notifier.sent[0] != "allow:X" {
		t.Errorf("expected 'allow:X', got %q", notifier.sent[0])
	}

	sentAllow, _ := st.IsSent("allow", "x")
	sentDeny, _ := st.IsSent("deny", "x")
	if !sentAllow {
		t.Errorf("allow chat not marked sent")
	}
	if sentDeny {
		t.Errorf("deny chat marked sent despite filter reject")
	}
}

// TestItemFilterChangeAfterReject verifies the documented behavior: a
// rejected (item, chat) pair CAN be re-evaluated on the next poll as long
// as the item is still in bot_seen - but since Runner filters by bot_seen
// first, the item will NOT be reconsidered on subsequent polls. This test
// pins that current behavior so we notice if it ever changes.
func TestItemFilterChangeAfterReject(t *testing.T) {
	st := newTestStore(t)
	_ = st.Subscribe("c1")

	src := &fakeSource{items: []core.Item{{ID: "x", Title: "X"}}}
	notifier := &fakeNotifier{}

	allow := false
	r := New(Config{
		Name: "test", Source: src, Formatter: fakeFormatter{},
		Notifier: notifier, Store: st, PollInterval: time.Hour,
		ItemFilter: func(ctx context.Context, chatID string, it core.Item) bool {
			return allow
		},
	})
	r.pollOnce(context.Background())
	if len(notifier.sent) != 0 {
		t.Errorf("first poll: expected 0 sends, got %d", len(notifier.sent))
	}

	// Flip filter to allow. On the next poll the Source will return the
	// same item, but bot_seen filtering in runner drops it before the
	// filter even sees it - so the subscriber still gets nothing.
	allow = true
	r.pollOnce(context.Background())
	if len(notifier.sent) != 0 {
		t.Errorf("second poll: item in bot_seen must not reappear, got %d sends", len(notifier.sent))
	}
}
