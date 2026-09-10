package bot

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bc1qwerty/txid-bot-framework/pkg/core"
)

// OnNewItem 의 계약은 "아이템당 한 번"이다(Config 필드 주석).
//
// ⚠ 발송 실패로 seen 처리가 미뤄진 아이템은 다음 폴에서 newItems 에 다시 들어오는데,
//   그때 훅이 또 불렸다. 실측: nara-bot 의 공고 하나가 차단된 구독자 때문에 8번
//   재시도되면서 **알림 허브에 같은 공고가 8번 푸시**됐다(2026-09-08~09, 발생 시각이
//   그 공고의 허브 푸시 시각과 정확히 일치). 알림 자체는 IsSent 가 막지만 이 훅은
//   그 게이트 밖이라 아무도 막지 않았다.

func TestOnNewItemFiresOncePerItemAcrossRetries(t *testing.T) {
	n := &flakyNotifier{failFirst: 2} // 1·2회차 실패 → 3회차 성공
	st := newTestStore(t)
	if err := st.Subscribe("100"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	hookCalls := 0
	r := New(Config{
		Name: "test", Source: &fakeSource{items: []core.Item{{ID: "x", Title: "Xray"}}},
		Formatter: fakeFormatter{}, Notifier: n, Store: st, PollInterval: time.Hour,
		OnNewItem: func(ctx context.Context, item core.Item) error {
			hookCalls++
			return nil
		},
	})

	r.PollOnce(context.Background()) // 실패 → 재시도 대기
	r.PollOnce(context.Background()) // 실패 → 재시도 대기
	r.PollOnce(context.Background()) // 성공

	if len(n.delivered) != 1 {
		t.Fatalf("결국 전달되지 않았다: %v", n.delivered)
	}
	if hookCalls != 1 {
		t.Fatalf("OnNewItem 이 %d회 불렸다 — 아이템당 한 번이어야 한다. "+
			"재시도마다 허브에 같은 항목이 다시 푸시된다.", hookCalls)
	}
}

// 서로 다른 아이템은 각각 한 번씩 불려야 한다 — 스킵이 너무 넓으면 알림이 누락된다.
func TestOnNewItemFiresForEachDistinctItem(t *testing.T) {
	st := newTestStore(t)
	if err := st.Subscribe("100"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	var got []string
	r := New(Config{
		Name: "test",
		Source: &fakeSource{items: []core.Item{
			{ID: "a", Title: "A"}, {ID: "b", Title: "B"},
		}},
		Formatter: fakeFormatter{}, Notifier: &fakeNotifier{}, Store: st, PollInterval: time.Hour,
		OnNewItem: func(ctx context.Context, item core.Item) error {
			got = append(got, item.ID)
			return nil
		},
	})

	r.PollOnce(context.Background())

	if len(got) != 2 {
		t.Fatalf("아이템 2건인데 훅이 %v 만 받았다", got)
	}
}

// 영구 실패로 구독을 끊은 아이템도 훅은 한 번만 — 그 경로는 seen 처리되므로
// 애초에 재시도되지 않지만, 계약을 고정해 둔다.
func TestOnNewItemOnceWhenRecipientIsPermanentlyDead(t *testing.T) {
	st := newTestStore(t)
	if err := st.Subscribe("100"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	hookCalls := 0
	r := New(Config{
		Name: "test", Source: &fakeSource{items: []core.Item{{ID: "x", Title: "Xray"}}},
		Formatter: fakeFormatter{}, Store: st, PollInterval: time.Hour,
		Notifier: &permaDeadNotifier{},
		OnNewItem: func(ctx context.Context, item core.Item) error {
			hookCalls++
			return nil
		},
	})

	r.PollOnce(context.Background())
	r.PollOnce(context.Background())

	if hookCalls != 1 {
		t.Fatalf("OnNewItem 이 %d회 불렸다", hookCalls)
	}
}

type permaDeadNotifier struct{}

func (permaDeadNotifier) Name() string { return "dead" }
func (permaDeadNotifier) Send(ctx context.Context, recipient string, msg core.Message) error {
	return errors.Join(core.ErrPermanentRecipient, errors.New("Forbidden: bot was blocked by the user"))
}
