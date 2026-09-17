package bot

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bc1qwerty/txid-bot-framework/pkg/core"
)

// OnNewItem 이 실패하면 **재시도되어야 한다** — 예전에는 두 방향으로 유실됐다.
//
// ⚠ ① 훅 실패 + 발송 성공: 아이템이 그대로 seen 처리돼 허브 푸시가 영영 유실됐다.
//
//	실측(nara-bot): 허브 페이싱으로 디스패치가 몇 분씩 걸리는 중 SIGTERM 이 오면
//	훅은 죽은 ctx 로 허브를 찔러 실패하고, 텔레그램 발송은 ctx 를 안 받아 성공했다.
//
// ⚠ ② 훅 실패 + 발송 실패: 재시도 폴에서 «발송 실패 카운터 0» 근사가 훅을 영영
//
//	건너뛰었다(그 근사는 8중 푸시 사고의 수리였는데 반대 방향으로 샜다).
//
// 성공 기록(bot_hook_done)으로 바꾸면 «아이템당 한 번» 과 «유실 없음» 이 동시에 선다.
func TestHookFailureWithSendSuccessRetriesHook(t *testing.T) {
	st := newTestStore(t)
	if err := st.Subscribe("100"); err != nil {
		t.Fatal(err)
	}
	n := &fakeNotifier{}
	hookCalls := 0
	r := New(Config{
		Name: "test", Source: &fakeSource{items: []core.Item{{ID: "x", Title: "Xray"}}},
		Formatter: fakeFormatter{}, Notifier: n, Store: st, PollInterval: time.Hour,
		OnNewItem: func(ctx context.Context, item core.Item) error {
			hookCalls++
			if hookCalls == 1 {
				return errors.New("hub 503")
			}
			return nil
		},
	})

	r.PollOnce(context.Background())
	if seen, _ := st.IsSeen("fake", "x"); seen {
		t.Fatal("훅이 실패했는데 seen 처리됐다 — 허브 푸시가 영영 유실된다(예전 결함 ①)")
	}
	r.PollOnce(context.Background())

	if hookCalls != 2 {
		t.Fatalf("훅이 %d회 불렸다 — 실패한 훅은 다음 폴에서 재시도돼야 한다", hookCalls)
	}
	if len(n.sent) != 1 {
		t.Fatalf("발송이 %d건 — 훅 재시도 폴에서 IsSent 게이트가 중복 발송을 막아야 한다", len(n.sent))
	}
	if seen, _ := st.IsSeen("fake", "x"); !seen {
		t.Fatal("훅이 성공했는데 여전히 unseen — 다음 폴마다 영원히 다시 돈다")
	}
}

// 훅이 계속 실패하면 발송 실패와 같은 상한(maxSendAttempts)에서 포기하고
// OnError 로 승격돼야 한다 — 조용히 영원히 재시도하는 것도, 조용히 버리는 것도 안 된다.
func TestHookFailureGivesUpAfterMaxAttempts(t *testing.T) {
	st := newTestStore(t)
	if err := st.Subscribe("100"); err != nil {
		t.Fatal(err)
	}
	n := &fakeNotifier{}
	hookCalls := 0
	var escalated []error
	r := New(Config{
		Name: "test", Source: &fakeSource{items: []core.Item{{ID: "x", Title: "Xray"}}},
		Formatter: fakeFormatter{}, Notifier: n, Store: st, PollInterval: time.Hour,
		OnNewItem: func(ctx context.Context, item core.Item) error {
			hookCalls++
			return errors.New("hub down")
		},
		OnError: func(err error) { escalated = append(escalated, err) },
	})

	for i := 0; i < maxSendAttempts; i++ {
		r.PollOnce(context.Background())
	}

	if hookCalls != maxSendAttempts {
		t.Fatalf("훅이 %d회 불렸다 (기대 %d) — 상한 없는 재시도거나 조기 포기다", hookCalls, maxSendAttempts)
	}
	if seen, _ := st.IsSeen("fake", "x"); !seen {
		t.Fatal("상한을 넘겼는데 unseen — 영원히 재시도한다")
	}
	if len(escalated) != 1 {
		t.Fatalf("포기가 OnError 로 %d회 승격됐다 (기대 1) — 조용히 버리는 것이 이 결함의 본질이었다", len(escalated))
	}
	if len(n.sent) != 1 {
		t.Fatalf("발송 %d건 — 훅 재시도 동안 텔레그램이 중복 발송되면 안 된다", len(n.sent))
	}
}
