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
