package bot

import (
	"context"
	"testing"
	"time"

	"github.com/bc1qwerty/txid-bot-framework/pkg/core"
)

// MultiSource dedup 네임스페이스 게이트.
//
// ⚠ 2026-09-08 사고: Runner 가 bot_seen 키로 cfg.Source.Name() 을 썼는데,
//   MultiSource 의 이름은 `multi[하위소스 목록]` 이라 **크롤러를 하나 켜거나 끄면
//   이름이 통째로 바뀐다.** 그러면 남은 소스 전부의 seen 이력이 고아가 되고,
//   다음 폴에서 백로그가 전부 신규로 보인 뒤 MaxItemsPerPoll 을 넘는 분량이
//   발송 없이 MarkSeen 되어 영구 유실된다.
//   safety_alarm_bot 의 라이브 DB 에 실제로 네임스페이스가 둘로 갈라져 있었다
//   (multi[moel] 160행 / multi[7개 소스] 80행). 2026-08-30 한 번의 실행에서
//   70행이 새 키로 들어갔는데 그중 실제 발송은 10건뿐이었다.
//   정작 봇의 주석은 ONLY/SKIP 스위치를 "재배포 없이 flaky 소스를 끄는 안전한
//   운영 수단" 이라고 안내하고 있었다.

type namedSource struct {
	name  string
	items []core.Item
}

func (s *namedSource) Name() string { return s.name }
func (s *namedSource) Fetch(ctx context.Context) ([]core.Item, error) {
	return s.items, nil
}

func TestMultiSourceDedupSurvivesSubSourceToggle(t *testing.T) {
	st := newTestStore(t)
	if err := st.Subscribe("100"); err != nil {
		t.Fatal(err)
	}
	notifier := &fakeNotifier{}

	a := &namedSource{name: "alpha", items: []core.Item{{ID: "a1", Title: "A1"}}}
	b := &namedSource{name: "bravo", items: []core.Item{{ID: "b1", Title: "B1"}}}

	// 1회차: 두 소스 모두 켜고 폴.
	New(Config{
		Name: "test", Source: core.NewMultiSource(a, b), Formatter: fakeFormatter{},
		Notifier: notifier, Store: st, PollInterval: time.Hour,
	}).PollOnce(context.Background())
	if len(notifier.sent) != 2 {
		t.Fatalf("1회차 발송 %d건, 2건이어야 한다", len(notifier.sent))
	}

	// 2회차: bravo 를 끈다(운영자가 SKIP 을 거는 상황) → MultiSource 이름이 바뀐다.
	// ⚠ 여기서 재발송 여부로는 결함을 못 잡는다 — bot_sent 는 소스와 무관하게
	//   (chat, item) 로 키가 잡혀 IsSent 게이트가 중복 발송을 막아 주기 때문이다.
	//   진짜 피해는 "이미 본 것이 다시 신규로 보이는 것" 이고, 그 상태에서
	//   MaxItemsPerPoll 을 넘는 분량이 **발송 없이 seen 처리돼 영구 유실**된다.
	//   그래서 store 의 seen 이력이 그대로 유효한지를 직접 본다.
	New(Config{
		Name: "test", Source: core.NewMultiSource(a), Formatter: fakeFormatter{},
		Notifier: notifier, Store: st, PollInterval: time.Hour,
	}).PollOnce(context.Background())

	seen, err := st.IsSeen("alpha", "a1")
	if err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("하위 소스를 끄자 alpha 의 seen 이력이 고아가 됐다 — 다음 폴에서 백로그가 전부 신규가 되고, 상한을 넘는 분량은 발송 없이 유실된다")
	}
}

// 네임스페이스가 갈리면 백로그가 신규로 보여 상한 초과분이 조용히 버려진다 —
// 그 손실 경로를 직접 재현한다.
func TestSubSourceToggleDoesNotSilentlyDropBacklog(t *testing.T) {
	st := newTestStore(t)
	_ = st.Subscribe("100")
	notifier := &fakeNotifier{}

	backlog := make([]core.Item, 0, 5)
	for _, id := range []string{"a1", "a2", "a3", "a4", "a5"} {
		backlog = append(backlog, core.Item{ID: id, Title: id})
	}
	a := &namedSource{name: "alpha", items: backlog}
	b := &namedSource{name: "bravo"}

	// 1회차: 상한 없이 전부 발송하고 seen 처리.
	New(Config{
		Name: "test", Source: core.NewMultiSource(a, b), Formatter: fakeFormatter{},
		Notifier: notifier, Store: st, PollInterval: time.Hour,
	}).PollOnce(context.Background())
	if len(notifier.sent) != 5 {
		t.Fatalf("1회차 발송 %d건, 5건이어야 한다", len(notifier.sent))
	}

	// 2회차: bravo 를 끄고 상한 2로 폴. 네임스페이스가 갈리면 5건이 전부 신규로
	// 보이고 상한 초과 3건이 발송 없이 seen 처리된다(= 영구 유실).
	r := New(Config{
		Name: "test", Source: core.NewMultiSource(a), Formatter: fakeFormatter{},
		Notifier: notifier, Store: st, PollInterval: time.Hour, MaxItemsPerPoll: 2,
	})
	r.PollOnce(context.Background())

	for _, id := range []string{"a3", "a4", "a5"} {
		seen, err := st.IsSeen("alpha", id)
		if err != nil {
			t.Fatal(err)
		}
		if !seen {
			t.Fatalf("%s 의 seen 이력이 사라졌다 — 네임스페이스가 갈렸다", id)
		}
	}
}

func TestMultiSourceStampsSubSourceName(t *testing.T) {
	a := &namedSource{name: "alpha", items: []core.Item{{ID: "a1"}}}
	b := &namedSource{name: "bravo", items: []core.Item{{ID: "b1"}}}
	items, err := core.NewMultiSource(a, b).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, it := range items {
		got[it.ID] = it.Source
	}
	if got["a1"] != "alpha" || got["b1"] != "bravo" {
		t.Fatalf("하위 소스 이름이 스탬프되지 않았다: %v", got)
	}
}

func TestSingleSourceDedupUnchanged(t *testing.T) {
	// 단일 소스 봇은 스탬프가 없으므로 예전처럼 소스 이름을 쓴다(회귀 방지).
	st := newTestStore(t)
	_ = st.Subscribe("100")
	notifier := &fakeNotifier{}
	src := &fakeSource{items: []core.Item{{ID: "x", Title: "Xray"}}}
	r := New(Config{
		Name: "test", Source: src, Formatter: fakeFormatter{},
		Notifier: notifier, Store: st, PollInterval: time.Hour,
	})
	r.PollOnce(context.Background())
	r.PollOnce(context.Background())
	if len(notifier.sent) != 1 {
		t.Fatalf("단일 소스에서 중복 발송: %v", notifier.sent)
	}
}
