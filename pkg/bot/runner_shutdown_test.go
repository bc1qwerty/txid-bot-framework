package bot

import (
	"context"
	"testing"
	"time"

	"github.com/bc1qwerty/txid-bot-framework/pkg/core"
)

// 종료(SIGTERM)로 ctx 가 끊긴 채 끝난 폴은 **장애가 아니다.**
//
// ⚠ 실측: VPS 재시작마다 social-feed 가 `fetch error: … context canceled` 를 찍고,
//   그 줄이 30분 감시를 타고 🔴 사람 경보로 올라갔다(2026-09-12). 더 나쁜 것은
//   그 다음 — 이미 죽은 ctx 로 디스패치를 계속해 OnNewItem 이 허브를 찌르다 실패했고
//   (nara-bot 2026-09-10 「허브 push 실패 23건」), 그 실패가 SendFailureAttempts 에
//   쌓이면 다음 기동에서 훅이 통째로 건너뛰어져 **허브 푸시가 영영 유실**된다.

// 취소된 ctx 로 Fetch 가 에러를 물고 돌아오면, OnError(사람 경보 경로)를 부르지 않는다.
func TestPollOnceDoesNotAlertOnShutdownCancel(t *testing.T) {
	st := newTestStore(t)
	if err := st.Subscribe("100"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	var onErrCalls int
	ctx, cancel := context.WithCancel(context.Background())
	r := New(Config{
		Name:      "test",
		Source:    &cancelingSource{cancel: cancel, items: []core.Item{{ID: "x", Title: "Xray"}}},
		Formatter: fakeFormatter{}, Notifier: &fakeNotifier{}, Store: st, PollInterval: time.Hour,
		OnError: func(err error) { onErrCalls++ },
	})

	r.PollOnce(ctx)

	if onErrCalls != 0 {
		t.Fatalf("종료로 끊긴 폴이 OnError 를 %d회 불렀다 — 재시작마다 사람 경보가 나간다.", onErrCalls)
	}
}

// 취소된 ctx 로는 아이템을 디스패치하지 않는다(훅도, 발송도).
// 아이템은 seen 처리되지 않으므로 다음 기동의 첫 폴이 정상적으로 다시 집는다.
func TestPollOnceSkipsDispatchOnShutdownCancel(t *testing.T) {
	st := newTestStore(t)
	if err := st.Subscribe("100"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	var hookCalls int
	n := &fakeNotifier{}
	ctx, cancel := context.WithCancel(context.Background())
	r := New(Config{
		Name:      "test",
		Source:    &cancelingSource{cancel: cancel, items: []core.Item{{ID: "x", Title: "Xray"}}},
		Formatter: fakeFormatter{}, Notifier: n, Store: st, PollInterval: time.Hour,
		OnNewItem: func(ctx context.Context, item core.Item) error { hookCalls++; return nil },
	})

	r.PollOnce(ctx)

	if hookCalls != 0 {
		t.Fatalf("종료 중인데 OnNewItem 이 %d회 불렸다 — 죽은 ctx 로 허브를 찌른다.", hookCalls)
	}
	if len(n.sent) != 0 {
		t.Fatalf("종료 중인데 %d건을 발송하려 했다: %v", len(n.sent), n.sent)
	}

	// 다음 기동이 다시 집을 수 있어야 한다 — 조용히 삼키면 안 된다.
	seen, err := st.IsSeen("fake", "x")
	if err != nil {
		t.Fatalf("is-seen: %v", err)
	}
	if seen {
		t.Fatal("종료 중 폴이 아이템을 seen 으로 박았다 — 그 알림은 영영 안 나간다.")
	}
}

// 살아있는 ctx 의 진짜 Fetch 에러는 그대로 경보해야 한다(게이트가 너무 넓으면
// 실제 장애가 조용해진다).
func TestPollOnceStillAlertsOnRealFetchError(t *testing.T) {
	st := newTestStore(t)
	var onErrCalls int
	r := New(Config{
		Name:      "test",
		Source:    &errSource{},
		Formatter: fakeFormatter{}, Notifier: &fakeNotifier{}, Store: st, PollInterval: time.Hour,
		OnError: func(err error) { onErrCalls++ },
	})

	r.PollOnce(context.Background())

	if onErrCalls != 1 {
		t.Fatalf("진짜 fetch 에러에 OnError 가 %d회 — 1회여야 한다.", onErrCalls)
	}
}

// 원샷 봇은 폴 전체를 `WithTimeout` 으로 감싼다(safety_alarm_bot·best-archive-bot
// 은 5분 예산 뒤 PollOnce). 예산 초과는 **사람이 봐야 하는 고장**이므로 종료 취소와
// 달리 반드시 경보해야 한다.
//
// ⚠이 게이트를 `ctx.Err() != nil` 로 두면 그 신호까지 통째로 삼킨다 — 크롤이 5분을
//
//	넘겨도 로그 한 줄 없이 조용해진다. 취소(Canceled)와 예산 초과(DeadlineExceeded)를
//	갈라야 하는 이유가 이것이다.
func TestPollOnceStillAlertsWhenDeadlineExceeded(t *testing.T) {
	st := newTestStore(t)
	var onErrCalls int
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	r := New(Config{
		Name:      "test",
		Source:    &deadlineSource{},
		Formatter: fakeFormatter{}, Notifier: &fakeNotifier{}, Store: st, PollInterval: time.Hour,
		OnError: func(err error) { onErrCalls++ },
	})

	r.PollOnce(ctx)

	if onErrCalls != 1 {
		t.Fatalf("예산 초과에 OnError 가 %d회 — 1회여야 한다. "+
			"원샷 봇의 「크롤이 예산을 넘겼다」가 조용해진다.", onErrCalls)
	}
}

// 예산이 끝날 때까지 붙잡고 있다가 그대로 돌려준다(원샷 봇의 실제 실패 모양).
type deadlineSource struct{}

func (deadlineSource) Name() string { return "fake" }
func (deadlineSource) Fetch(ctx context.Context) ([]core.Item, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// SIGTERM 이 «디스패치 도중» 도착한 상황: 아이템 a 발송 중 ctx 가 끊긴다.
// Fetch 직후 게이트는 이미 지났고, 텔레그램 발송은 ctx 를 안 받아 성공하므로
// 게이트가 루프 안에도 없으면 b 는 죽은 ctx 로 훅을 타고 seen 처리까지 돼
// 허브 푸시가 영영 유실된다. 남은 아이템은 다음 기동의 첫 폴이 다시 집는다.
func TestPollOnceStopsDispatchMidLoopOnShutdownCancel(t *testing.T) {
	st := newTestStore(t)
	if err := st.Subscribe("100"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	var hooked []string
	ctx, cancel := context.WithCancel(context.Background())
	n := &cancelingNotifier{cancel: cancel}
	r := New(Config{
		Name:      "test",
		Source:    &fakeSource{items: []core.Item{{ID: "a", Title: "Alpha"}, {ID: "b", Title: "Bravo"}}},
		Formatter: fakeFormatter{}, Notifier: n, Store: st, PollInterval: time.Hour,
		OnNewItem: func(ctx context.Context, item core.Item) error {
			hooked = append(hooked, item.ID)
			return nil
		},
	})

	r.PollOnce(ctx)

	if len(hooked) != 1 || hooked[0] != "a" {
		t.Fatalf("종료 후 아이템에도 OnNewItem 이 불렸다: %v — 죽은 ctx 로 허브를 찌른다.", hooked)
	}
	if len(n.sent) != 1 {
		t.Fatalf("종료 후에도 발송하려 했다: %v", n.sent)
	}
	// a 는 정상 완료라 seen, b 는 다음 기동이 다시 집어야 한다.
	if seen, err := st.IsSeen("fake", "a"); err != nil || !seen {
		t.Fatalf("정상 발송된 a 가 seen 이 아니다 (err=%v)", err)
	}
	seen, err := st.IsSeen("fake", "b")
	if err != nil {
		t.Fatalf("is-seen: %v", err)
	}
	if seen {
		t.Fatal("종료로 못 보낸 b 가 seen 으로 박혔다 — 그 알림과 허브 푸시는 영영 안 나간다.")
	}
}

// 첫 발송 «도중» SIGTERM 이 오는 상황. 발송 자체는 성공으로 돌려준다 —
// tgbotapi 의 Send 는 ctx 를 모르기 때문에 실제로도 그렇게 된다.
type cancelingNotifier struct {
	cancel context.CancelFunc
	sent   []string
}

func (c *cancelingNotifier) Name() string { return "fake" }
func (c *cancelingNotifier) Send(ctx context.Context, recipient string, msg core.Message) error {
	c.sent = append(c.sent, recipient+":"+msg.Text)
	c.cancel()
	return nil
}

// SIGTERM 이 폴 도중에 도착한 상황: Fetch 가 도는 사이 ctx 가 끊기고,
// 그 상태 그대로 아이템과 에러가 돌아온다(social-feed 의 실제 로그 모양이다 —
// `fetch error: … context canceled` 다음 줄이 `dispatching 10 items despite errors`).
type cancelingSource struct {
	cancel context.CancelFunc
	items  []core.Item
}

func (c *cancelingSource) Name() string { return "fake" }
func (c *cancelingSource) Fetch(ctx context.Context) ([]core.Item, error) {
	c.cancel()
	return c.items, context.Canceled
}

type errSource struct{}

func (errSource) Name() string { return "fake" }
func (errSource) Fetch(ctx context.Context) ([]core.Item, error) {
	return nil, context.DeadlineExceeded
}
