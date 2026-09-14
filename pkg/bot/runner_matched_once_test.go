package bot

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bc1qwerty/txid-bot-framework/pkg/core"
)

// OnItemMatched 도 OnNewItem 처럼 "아이템당 한 번"이어야 한다(Config 필드 주석).
//
// ⚠ v0.10.0 이 OnNewItem 에만 재시도 중복 게이트를 넣었고 이 훅에는 없었다.
//   부분 발송 실패로 seen 이 미뤄진 아이템이 다음 폴에서 남은 구독자에게 발송
//   성공하면 matched 가 또 true 가 돼 허브에 같은 아이템이 다시 푸시됐다 —
//   nara-bot 8중 푸시와 같은 모양의 형제 분기.

// recipientFlakyNotifier 는 수신자별로 첫 N회 호출만 실패한다.
type recipientFlakyNotifier struct {
	failFirst map[string]int
	calls     map[string]int
	delivered []string
}

func (n *recipientFlakyNotifier) Name() string { return "rflaky" }
func (n *recipientFlakyNotifier) Send(ctx context.Context, recipient string, msg core.Message) error {
	n.calls[recipient]++
	if n.calls[recipient] <= n.failFirst[recipient] {
		return errors.New("Too Many Requests: retry after 30")
	}
	n.delivered = append(n.delivered, recipient+":"+msg.Text)
	return nil
}

func TestOnItemMatchedFiresOnceAcrossRetryPolls(t *testing.T) {
	st := newTestStore(t)
	if err := st.Subscribe("100"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := st.Subscribe("200"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	n := &recipientFlakyNotifier{
		failFirst: map[string]int{"200": 1}, // 200 은 1회차만 실패
		calls:     map[string]int{},
	}
	hookCalls := 0
	r := New(Config{
		Name: "test", Source: &fakeSource{items: []core.Item{{ID: "x", Title: "Xray"}}},
		Formatter: fakeFormatter{}, Notifier: n, Store: st, PollInterval: time.Hour,
		OnItemMatched: func(ctx context.Context, item core.Item) error {
			hookCalls++
			return nil
		},
	})

	r.PollOnce(context.Background()) // 100 성공(훅 1회), 200 실패 → seen 보류
	r.PollOnce(context.Background()) // 200 재시도 성공 — 훅이 또 불리면 허브 중복 푸시

	if len(n.delivered) != 2 {
		t.Fatalf("두 구독자 모두에게 결국 전달돼야 한다: %v", n.delivered)
	}
	if hookCalls != 1 {
		t.Fatalf("OnItemMatched 가 %d회 불렸다 — 재시도 폴마다 허브에 같은 아이템이 다시 푸시된다.", hookCalls)
	}
}

// «전원 실패 후 성공»의 첫 발화는 살아야 한다 — OnNewItem 의 SendFailureAttempts
// 게이트를 그대로 복사하면 이 케이스의 푸시가 통째로 유실된다.
func TestOnItemMatchedStillFiresWhenFirstPollAllFailed(t *testing.T) {
	st := newTestStore(t)
	if err := st.Subscribe("100"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	n := &flakyNotifier{failFirst: 1}
	hookCalls := 0
	r := New(Config{
		Name: "test", Source: &fakeSource{items: []core.Item{{ID: "x", Title: "Xray"}}},
		Formatter: fakeFormatter{}, Notifier: n, Store: st, PollInterval: time.Hour,
		OnItemMatched: func(ctx context.Context, item core.Item) error {
			hookCalls++
			return nil
		},
	})

	r.PollOnce(context.Background()) // 전원 실패 — matched 없음, 훅 0회
	r.PollOnce(context.Background()) // 재시도 성공 — 여기서 1회는 나야 한다

	if hookCalls != 1 {
		t.Fatalf("OnItemMatched 가 %d회 불렸다 — 첫 성공 폴에서 정확히 1회여야 한다.", hookCalls)
	}
}
