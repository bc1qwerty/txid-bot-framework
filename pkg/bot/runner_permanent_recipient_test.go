package bot

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/bc1qwerty/txid-bot-framework/pkg/core"
	"github.com/bc1qwerty/txid-bot-framework/pkg/store"
)

// 차단된 수신자 처리.
//
// ⚠ 2026-09-09 사고: nara-bot 저널이 `send error … Forbidden: bot was blocked by
//   the user` 로 계속 채워지고 있었다. 재시도 상한(maxSendAttempts)은 **아이템
//   단위**라 새 공고가 올 때마다 카운터가 0 부터 다시 시작한다 — 상한이 있어도
//   영구 실패한 구독 하나가 영원히 소음을 낸다. 상한은 "한 아이템을 몇 번까지"를
//   막을 뿐 "못 보내는 구독자를 언제 끊을지"는 아무도 결정하지 않았다.

// blockedNotifier 는 특정 수신자에게만 영구 실패를 돌려준다.
type blockedNotifier struct {
	blocked   map[string]bool
	calls     map[string]int
	delivered []string
}

func newBlockedNotifier(blocked ...string) *blockedNotifier {
	b := &blockedNotifier{blocked: map[string]bool{}, calls: map[string]int{}}
	for _, r := range blocked {
		b.blocked[r] = true
	}
	return b
}

func (b *blockedNotifier) Name() string { return "blocked" }
func (b *blockedNotifier) Send(ctx context.Context, recipient string, msg core.Message) error {
	b.calls[recipient]++
	if b.blocked[recipient] {
		// 실제 notify.Telegram 이 올리는 형태와 같게 감싼다.
		return fmt.Errorf("%w: Forbidden: bot was blocked by the user", core.ErrPermanentRecipient)
	}
	b.delivered = append(b.delivered, recipient+":"+msg.Text)
	return nil
}

func runnerWithSubs(t *testing.T, n core.Notifier, onErr func(error), subs ...store.Subscription) (*Runner, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	for _, s := range subs {
		if err := st.SubscribeRich(s); err != nil {
			t.Fatalf("subscribe %s: %v", s.ID, err)
		}
	}
	cfg := Config{
		Name: "test", Source: &fakeSource{items: []core.Item{{ID: "x", Title: "Xray"}}},
		Formatter: fakeFormatter{}, Notifier: n, Store: st, PollInterval: time.Hour,
	}
	if onErr != nil {
		cfg.OnError = func(err error) { onErr(err) }
	}
	return New(cfg), st
}

func TestPermanentRecipientIsDeactivatedNotRetried(t *testing.T) {
	n := newBlockedNotifier("100")
	var errs []error
	r, st := runnerWithSubs(t, n, func(e error) { errs = append(errs, e) },
		store.Subscription{ID: "100", Recipient: "100"})

	for i := 0; i < 5; i++ {
		r.PollOnce(context.Background())
	}

	// 한 번 시도하고 끊었어야 한다. 예전 코드는 폴마다 계속 두드렸다.
	if n.calls["100"] != 1 {
		t.Fatalf("차단된 수신자에게 %d회 시도했다 — 1회 뒤 끊어야 한다", n.calls["100"])
	}
	subs, err := st.ActiveSubscriptions()
	if err != nil {
		t.Fatalf("ActiveSubscriptions: %v", err)
	}
	if len(subs) != 0 {
		t.Fatalf("구독이 아직 살아 있다: %v", subs)
	}
	if len(errs) == 0 {
		t.Fatal("구독을 껐으면서 OnError 를 부르지 않았다 — 사람이 알 방법이 없다")
	}
}

// ⚠차단된 구독자 하나가 **다른 구독자의 알림까지 막으면 안 된다.** 영구 실패를
// sendFailed 로 세면 아이템이 seen 처리되지 않아 멀쩡한 구독자에게 같은 알림이
// 매 폴마다 다시 간다(IsSent 게이트가 막긴 하지만 아이템이 영원히 큐에 남는다).
func TestPermanentRecipientDoesNotAffectOthers(t *testing.T) {
	n := newBlockedNotifier("100")
	r, st := runnerWithSubs(t, n, nil,
		store.Subscription{ID: "100", Recipient: "100"},
		store.Subscription{ID: "200", Recipient: "200"})

	r.PollOnce(context.Background())
	r.PollOnce(context.Background())

	if len(n.delivered) != 1 || n.delivered[0] != "200:Xray" {
		t.Fatalf("멀쩡한 구독자에게 정확히 1회 전달돼야 한다: %v", n.delivered)
	}
	subs, _ := st.ActiveSubscriptions()
	if len(subs) != 1 || subs[0].ID != "200" {
		t.Fatalf("살아 있어야 할 구독만 남지 않았다: %v", subs)
	}
}

// nara-bot 처럼 한 사람이 조건별로 여러 슬롯을 갖는 봇에서는, 차단 한 번에
// 그 사람의 **모든 슬롯**이 꺼져야 한다. chat_id 하나만 끄면 나머지가 계속 운다.
func TestPermanentRecipientDeactivatesEverySlot(t *testing.T) {
	n := newBlockedNotifier("5385429383")
	r, st := runnerWithSubs(t, n, nil,
		store.Subscription{ID: "5385429383:1", Recipient: "5385429383"},
		store.Subscription{ID: "5385429383:2", Recipient: "5385429383"},
		store.Subscription{ID: "999", Recipient: "999"})

	r.PollOnce(context.Background())

	subs, _ := st.ActiveSubscriptions()
	if len(subs) != 1 || subs[0].Recipient != "999" {
		t.Fatalf("차단된 수신자의 슬롯이 전부 꺼지지 않았다: %v", subs)
	}
	if n.calls["5385429383"] != 1 {
		t.Fatalf("같은 폴에서 슬롯마다 다시 시도했다: %d회", n.calls["5385429383"])
	}
}

// 일시적 실패는 여전히 재시도돼야 한다 — 영구 판정이 넓어져 멀쩡한 구독자를
// 끊는 것이 이 변경의 유일한 위험이다.
func TestTransientFailureStillRetriesAndKeepsSubscription(t *testing.T) {
	n := &flakyNotifier{failFirst: 1}
	r, st := runnerWithSubs(t, n, nil, store.Subscription{ID: "100", Recipient: "100"})

	r.PollOnce(context.Background())
	subs, _ := st.ActiveSubscriptions()
	if len(subs) != 1 {
		t.Fatalf("일시 실패로 구독이 꺼졌다: %v", subs)
	}
	r.PollOnce(context.Background())
	if len(n.delivered) != 1 {
		t.Fatalf("일시 실패가 재시도되지 않았다: %v", n.delivered)
	}
}
