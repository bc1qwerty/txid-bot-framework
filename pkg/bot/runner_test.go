package bot

import (
	"context"
	"strconv"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

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
	r.PollOnce(context.Background())
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
	r.PollOnce(context.Background())

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
		ItemFilter: func(ctx context.Context, sub store.Subscription, it core.Item) bool {
			return false
		},
	})
	r.PollOnce(context.Background())

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
		ItemFilter: func(ctx context.Context, sub store.Subscription, it core.Item) bool {
			return sub.ID == "allow"
		},
	})
	r.PollOnce(context.Background())

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
		ItemFilter: func(ctx context.Context, sub store.Subscription, it core.Item) bool {
			return allow
		},
	})
	r.PollOnce(context.Background())
	if len(notifier.sent) != 0 {
		t.Errorf("first poll: expected 0 sends, got %d", len(notifier.sent))
	}

	// Flip filter to allow. On the next poll the Source will return the
	// same item, but bot_seen filtering in runner drops it before the
	// filter even sees it - so the subscriber still gets nothing.
	allow = true
	r.PollOnce(context.Background())
	if len(notifier.sent) != 0 {
		t.Errorf("second poll: item in bot_seen must not reappear, got %d sends", len(notifier.sent))
	}
}

// subFormatter records which (sub, item) pair it was asked to format and
// returns a sub-specific message. Used to exercise the SubscriberFormatter
// interface path in the runner.
type subFormatter struct {
	calls []string
}

func (s *subFormatter) Format(it core.Item) core.Message {
	return core.Message{Text: "base:" + it.Title}
}

func (s *subFormatter) FormatFor(sub store.Subscription, it core.Item) core.Message {
	s.calls = append(s.calls, sub.ID+"/"+it.ID)
	return core.Message{Text: sub.ID + ":" + it.Title}
}

// TestSubscribeRichMetaRoundTrip verifies Meta survives a write/read cycle
// and that ActiveSubscriptions returns the same ID/Recipient/Meta.
func TestSubscribeRichMetaRoundTrip(t *testing.T) {
	st := newTestStore(t)
	err := st.SubscribeRich(store.Subscription{
		ID:        "user1:cond42",
		Recipient: "user1",
		Meta: map[string]string{
			"sigungu":   "11680",
			"min_price": "500000000",
		},
	})
	if err != nil {
		t.Fatalf("SubscribeRich: %v", err)
	}

	subs, err := st.ActiveSubscriptions()
	if err != nil {
		t.Fatalf("ActiveSubscriptions: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1 sub, got %d", len(subs))
	}
	got := subs[0]
	if got.ID != "user1:cond42" {
		t.Errorf("ID: got %q, want %q", got.ID, "user1:cond42")
	}
	if got.Recipient != "user1" {
		t.Errorf("Recipient: got %q, want %q", got.Recipient, "user1")
	}
	if got.Meta["sigungu"] != "11680" || got.Meta["min_price"] != "500000000" {
		t.Errorf("Meta: got %+v", got.Meta)
	}
}

// TestLegacySubscribeBackwardCompat verifies plain Subscribe(chatID) still
// produces a Subscription whose ID and Recipient are both the chat_id and
// Meta is empty.
func TestLegacySubscribeBackwardCompat(t *testing.T) {
	st := newTestStore(t)
	if err := st.Subscribe("12345"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	subs, err := st.ActiveSubscriptions()
	if err != nil {
		t.Fatalf("ActiveSubscriptions: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("expected 1 sub, got %d", len(subs))
	}
	got := subs[0]
	if got.ID != "12345" || got.Recipient != "12345" {
		t.Errorf("legacy sub should have ID=Recipient=chat_id, got ID=%q Recipient=%q", got.ID, got.Recipient)
	}
	if len(got.Meta) != 0 {
		t.Errorf("legacy sub should have empty Meta, got %+v", got.Meta)
	}

	// Legacy ActiveSubscribers() must still work.
	recipients, err := st.ActiveSubscribers()
	if err != nil {
		t.Fatalf("ActiveSubscribers: %v", err)
	}
	if len(recipients) != 1 || recipients[0] != "12345" {
		t.Errorf("ActiveSubscribers: %v", recipients)
	}
}

// TestCompositeSubscriptionIDs verifies the bangool-style pattern: one
// recipient (chat_id) can have multiple subscriptions with different IDs,
// each tracked independently in bot_sent.
func TestCompositeSubscriptionIDs(t *testing.T) {
	st := newTestStore(t)
	_ = st.SubscribeRich(store.Subscription{ID: "100:cond1", Recipient: "100"})
	_ = st.SubscribeRich(store.Subscription{ID: "100:cond2", Recipient: "100"})

	src := &fakeSource{items: []core.Item{{ID: "tx1", Title: "Transaction 1"}}}
	notifier := &fakeNotifier{}

	r := New(Config{
		Name: "test", Source: src, Formatter: fakeFormatter{},
		Notifier: notifier, Store: st, PollInterval: time.Hour,
	})
	r.PollOnce(context.Background())

	if len(notifier.sent) != 2 {
		t.Fatalf("expected 2 sends (one per sub) to same recipient, got %d: %v", len(notifier.sent), notifier.sent)
	}
	// Both should go to recipient "100"
	for _, rec := range notifier.sent {
		if rec[:4] != "100:" {
			t.Errorf("send should target recipient 100, got %q", rec)
		}
	}

	// Both subs should be marked sent for tx1, independently.
	sent1, _ := st.IsSent("100:cond1", "tx1")
	sent2, _ := st.IsSent("100:cond2", "tx1")
	if !sent1 || !sent2 {
		t.Errorf("both subs should be marked sent: cond1=%v cond2=%v", sent1, sent2)
	}
}

// TestSubscriberFormatterInterface verifies that a Formatter implementing
// SubscriberFormatter gets called with each subscription and its output is
// used instead of the base Format(item).
func TestSubscriberFormatterInterface(t *testing.T) {
	st := newTestStore(t)
	_ = st.SubscribeRich(store.Subscription{ID: "a", Recipient: "a"})
	_ = st.SubscribeRich(store.Subscription{ID: "b", Recipient: "b"})

	src := &fakeSource{items: []core.Item{{ID: "i1", Title: "Alpha"}}}
	notifier := &fakeNotifier{}
	fmt := &subFormatter{}

	r := New(Config{
		Name: "test", Source: src, Formatter: fmt,
		Notifier: notifier, Store: st, PollInterval: time.Hour,
	})
	r.PollOnce(context.Background())

	if len(fmt.calls) != 2 {
		t.Fatalf("FormatFor should be called twice, got %d: %v", len(fmt.calls), fmt.calls)
	}
	// Messages should be sub-specific (not the base "base:Alpha" text).
	if len(notifier.sent) != 2 {
		t.Fatalf("expected 2 sends, got %d", len(notifier.sent))
	}
	// Each sub sees its own ID baked into the message.
	wantA := "a:a:Alpha"
	wantB := "b:b:Alpha"
	gotA, gotB := notifier.sent[0], notifier.sent[1]
	// order isn't guaranteed, just check both are present
	if !(gotA == wantA || gotB == wantA) {
		t.Errorf("missing %q in %v", wantA, notifier.sent)
	}
	if !(gotA == wantB || gotB == wantB) {
		t.Errorf("missing %q in %v", wantB, notifier.sent)
	}
}

// TestItemFilterWithSubscriptionMeta verifies a filter can consume Meta
// from a Subscription to make per-sub decisions without external lookups.
func TestItemFilterWithSubscriptionMeta(t *testing.T) {
	st := newTestStore(t)
	_ = st.SubscribeRich(store.Subscription{
		ID: "sub1", Recipient: "chat1",
		Meta: map[string]string{"min_price": "100"},
	})
	_ = st.SubscribeRich(store.Subscription{
		ID: "sub2", Recipient: "chat2",
		Meta: map[string]string{"min_price": "1000"},
	})

	// Item carries a "price" in Meta too.
	src := &fakeSource{items: []core.Item{{
		ID: "tx1", Title: "500 deal",
		Meta: map[string]string{"price": "500"},
	}}}
	notifier := &fakeNotifier{}

	r := New(Config{
		Name: "test", Source: src, Formatter: fakeFormatter{},
		Notifier: notifier, Store: st, PollInterval: time.Hour,
		ItemFilter: func(ctx context.Context, sub store.Subscription, it core.Item) bool {
			itemPrice, _ := strconv.Atoi(it.Meta["price"])
			minPrice, _ := strconv.Atoi(sub.Meta["min_price"])
			return itemPrice >= minPrice
		},
	})
	r.PollOnce(context.Background())

	if len(notifier.sent) != 1 {
		t.Fatalf("expected 1 send (sub1 accepts, sub2 rejects), got %d: %v", len(notifier.sent), notifier.sent)
	}
	if notifier.sent[0][:5] != "chat1" {
		t.Errorf("expected chat1 recipient, got %q", notifier.sent[0])
	}
}

// TestHandleTextRegistration verifies the TextHandler hook plumbing.
// We can't easily fake tgbotapi's update channel, so this test only
// pins the surface API: the handler can be registered, replaced with
// nil, and re-registered without panicking. End-to-end dispatch is
// exercised via integration runs (food-recall + bangool).
func TestHandleTextRegistration(t *testing.T) {
	st := newTestStore(t)
	// notify.NewTelegram needs a real token; we cannot construct a
	// dispatcher without one in unit tests. Build the struct directly
	// to verify the API shape.
	d := &TelegramDispatcher{
		store:     st,
		handlers:  make(map[string]CommandHandler),
		callbacks: make(map[string]CallbackHandler),
	}

	called := false
	d.HandleText(func(ctx context.Context, msg *tgbotapi.Message) string {
		called = true
		return "ok"
	})
	if d.textHandler == nil {
		t.Errorf("HandleText did not register")
	}
	// Replacing with nil should clear.
	d.HandleText(nil)
	if d.textHandler != nil {
		t.Errorf("nil HandleText should clear")
	}
	_ = called
}

// fakeErrSource always returns the same error so we can verify the
// ErrorThrottle behavior in invokeOnError.
type fakeErrSource struct {
	err error
}

func (f *fakeErrSource) Name() string                                   { return "errsrc" }
func (f *fakeErrSource) Fetch(ctx context.Context) ([]core.Item, error) { return nil, f.err }

// TestErrorThrottleSuppressesDuplicates verifies that two consecutive
// PollOnce calls hitting the same error fire OnError only once when
// ErrorThrottle is set.
func TestErrorThrottleSuppressesDuplicates(t *testing.T) {
	st := newTestStore(t)
	src := &fakeErrSource{err: errFake("nope")}
	notifier := &fakeNotifier{}

	var fired int
	r := New(Config{
		Name: "test", Source: src, Formatter: fakeFormatter{},
		Notifier: notifier, Store: st, PollInterval: time.Hour,
		ErrorThrottle: time.Hour,
		OnError:       func(err error) { fired++ },
	})

	r.PollOnce(context.Background())
	r.PollOnce(context.Background())
	r.PollOnce(context.Background())

	if fired != 1 {
		t.Errorf("expected 1 OnError fire (3 polls coalesced), got %d", fired)
	}
}

// TestErrorThrottleAllowsDifferentErrors verifies that distinct error
// strings break through the throttle (each fires once).
func TestErrorThrottleAllowsDifferentErrors(t *testing.T) {
	st := newTestStore(t)
	src := &fakeErrSource{}
	notifier := &fakeNotifier{}

	var fired []string
	r := New(Config{
		Name: "test", Source: src, Formatter: fakeFormatter{},
		Notifier: notifier, Store: st, PollInterval: time.Hour,
		ErrorThrottle: time.Hour,
		OnError:       func(err error) { fired = append(fired, err.Error()) },
	})

	src.err = errFake("first")
	r.PollOnce(context.Background())
	src.err = errFake("second")
	r.PollOnce(context.Background())
	src.err = errFake("first") // back to first - within window so suppressed
	r.PollOnce(context.Background())

	// last fire was "second", lastErrMsg="second". Now we get "first"
	// which differs from "second" so it fires.
	if len(fired) != 3 {
		t.Errorf("expected 3 fires (each distinct), got %d: %v", len(fired), fired)
	}
}

// TestErrorThrottleDisabledByDefault verifies that with ErrorThrottle=0
// every error fires (no coalescing) - existing behavior.
func TestErrorThrottleDisabledByDefault(t *testing.T) {
	st := newTestStore(t)
	src := &fakeErrSource{err: errFake("always")}
	notifier := &fakeNotifier{}

	var fired int
	r := New(Config{
		Name: "test", Source: src, Formatter: fakeFormatter{},
		Notifier: notifier, Store: st, PollInterval: time.Hour,
		// ErrorThrottle: 0 (default)
		OnError: func(err error) { fired++ },
	})

	r.PollOnce(context.Background())
	r.PollOnce(context.Background())

	if fired != 2 {
		t.Errorf("default throttle should fire every error, got %d", fired)
	}
}

// errFake is a tiny error type so we don't drag in fmt.Errorf each time.
type errFake string

func (e errFake) Error() string { return string(e) }
