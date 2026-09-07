package bot

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bc1qwerty/txid-bot-framework/pkg/core"
)

// 발송 실패 재시도 게이트.
//
// ⚠ 2026-09-08 사고: Notifier.Send 가 에러를 내면 로그만 찍고 continue 했는데,
//   아이템 루프 끝의 MarkSeen 은 발송 성공 여부와 무관하게 무조건 실행됐다.
//   다음 폴에서는 IsSeen 게이트가 그 아이템을 걸러내므로 **재시도 기회가 영영
//   없었다.** 텔레그램 429·네트워크 오류·업로드 실패가 전부 이 경로로 떨어졌고
//   흔적은 로그 한 줄뿐이었다(MarkSent 도 안 남아 추적 불가).
//   이 프레임워크를 쓰는 모든 봇에 공통이었고, 그런데도 runner_test.go 에는
//   Send 가 에러를 반환하는 테스트가 **하나도 없었다**.

// 첫 N회 호출만 실패하는 notifier.
type flakyNotifier struct {
	failFirst int
	calls     int
	delivered []string
}

func (f *flakyNotifier) Name() string { return "flaky" }
func (f *flakyNotifier) Send(ctx context.Context, recipient string, msg core.Message) error {
	f.calls++
	if f.calls <= f.failFirst {
		return errors.New("Too Many Requests: retry after 30")
	}
	f.delivered = append(f.delivered, recipient+":"+msg.Text)
	return nil
}

// 언제나 실패하는 notifier(영구 실패 흉내).
type deadNotifier struct{ calls int }

func (d *deadNotifier) Name() string { return "dead" }
func (d *deadNotifier) Send(ctx context.Context, recipient string, msg core.Message) error {
	d.calls++
	return errors.New("Forbidden: bot was blocked by the user")
}

func newRunner(t *testing.T, n core.Notifier, onErr func(error)) *Runner {
	t.Helper()
	st := newTestStore(t)
	if err := st.Subscribe("100"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	src := &fakeSource{items: []core.Item{{ID: "x", Title: "Xray"}}}
	cfg := Config{
		Name: "test", Source: src, Formatter: fakeFormatter{},
		Notifier: n, Store: st, PollInterval: time.Hour,
	}
	if onErr != nil {
		cfg.OnError = func(err error) { onErr(err) }
	}
	return New(cfg)
}

func TestSendFailureIsRetriedOnNextPoll(t *testing.T) {
	n := &flakyNotifier{failFirst: 1}
	r := newRunner(t, n, nil)

	r.PollOnce(context.Background())
	if len(n.delivered) != 0 {
		t.Fatalf("1회차는 실패해야 한다: %v", n.delivered)
	}

	// 소스는 같은 아이템을 계속 준다. 예전 코드는 여기서 'no new items' 로 끝났다.
	r.PollOnce(context.Background())
	if len(n.delivered) != 1 {
		t.Fatalf("2회차에 재발송돼야 한다 — 실패한 알림이 영구 유실됐다 (calls=%d delivered=%v)", n.calls, n.delivered)
	}
	if n.delivered[0] != "100:Xray" {
		t.Fatalf("잘못된 내용 전달: %v", n.delivered)
	}
}

func TestSuccessfulSendIsNotResent(t *testing.T) {
	n := &flakyNotifier{failFirst: 0}
	r := newRunner(t, n, nil)

	r.PollOnce(context.Background())
	r.PollOnce(context.Background())
	r.PollOnce(context.Background())
	if len(n.delivered) != 1 {
		t.Fatalf("성공한 알림이 중복 발송됐다: %v", n.delivered)
	}
}

func TestPermanentFailureGivesUpAndEscalates(t *testing.T) {
	n := &deadNotifier{}
	var errs []error
	r := newRunner(t, n, func(e error) { errs = append(errs, e) })

	// 상한(maxSendAttempts)을 넘길 때까지 돌린다. 무한 재시도가 아니어야 한다.
	for i := 0; i < maxSendAttempts+3; i++ {
		r.PollOnce(context.Background())
	}
	if n.calls > maxSendAttempts {
		t.Fatalf("상한을 넘겨 재시도했다: %d회 (상한 %d)", n.calls, maxSendAttempts)
	}
	if n.calls != maxSendAttempts {
		t.Fatalf("상한까지 재시도하지 않았다: %d회", n.calls)
	}
	// ⚠ 포기할 때는 조용하면 안 된다 — 조용히 버리는 것이 이 결함의 본질이었다.
	if len(errs) == 0 {
		t.Fatal("포기하면서 OnError 를 부르지 않았다 — 관리자가 알 방법이 없다")
	}
}
