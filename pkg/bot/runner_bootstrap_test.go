package bot

import (
	"context"
	"testing"
	"time"

	"github.com/bc1qwerty/txid-bot-framework/pkg/core"
)

// BootstrapMode 불변식 게이트.
//
// dedup 상태를 잃었을 때(빈 DB) 봇을 그냥 띄우면 크롤한 글이 전부 신규로 보이고,
// MaxItemsPerPoll 을 넘는 분량은 **발송 없이 seen 처리돼 영구 유실**된다.
// 2026-09-08 safety_alarm 에서 실제로 7건이 그렇게 사라졌다.
//
// BootstrapMode 는 그 상황의 복구 수단이다 — 한 주기 분량의 글을 포기하는 대신
// 대량 중복 발송도, 조용한 유실도 만들지 않는다. best-archive-bot 의 GHA 는
// dedup DB 캐시가 축출되면 이 모드로 돌도록 돼 있어서(bot.yml "Detect dedup cache
// loss"), 아래 두 성질이 깨지면 그 복구 경로가 통째로 무너진다:
//
//	1) 발송이 0건이다 — 하나라도 나가면 캐시 유실 때마다 대량 발송이 된다.
//	2) 상한과 무관하게 **전부** seen 처리된다 — 일부만 표시하면 남은 것이
//	   다음 실행에서 다시 신규가 되어, 정확히 막으려던 유실이 그때 일어난다.
func TestBootstrapMarksAllSeenAndSendsNothing(t *testing.T) {
	st := newTestStore(t)
	if err := st.Subscribe("100"); err != nil {
		t.Fatal(err)
	}
	notifier := &fakeNotifier{}

	items := make([]core.Item, 0, 25)
	for _, id := range []string{
		"i01", "i02", "i03", "i04", "i05", "i06", "i07", "i08", "i09", "i10",
		"i11", "i12", "i13", "i14", "i15", "i16", "i17", "i18", "i19", "i20",
		"i21", "i22", "i23", "i24", "i25",
	} {
		items = append(items, core.Item{ID: id, Title: id})
	}
	src := &fakeSource{items: items}

	// ⚠ 상한을 일부러 항목 수보다 훨씬 작게 잡는다. 부트스트랩이 상한을 존중해
	//   버리면 나머지 20건이 다음 실행에서 신규가 되고, 그게 바로 유실 경로다.
	r := New(Config{
		Name: "test", Source: src, Formatter: fakeFormatter{},
		Notifier: notifier, Store: st, PollInterval: time.Hour,
		MaxItemsPerPoll: 5,
		BootstrapMode:   true,
	})
	r.PollOnce(context.Background())

	if len(notifier.sent) != 0 {
		t.Fatalf("부트스트랩인데 %d건이 발송됐다 — 캐시 유실 때마다 대량 발송이 된다", len(notifier.sent))
	}
	for _, it := range items {
		seen, err := st.IsSeen(src.Name(), it.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !seen {
			t.Fatalf("%s 가 seen 처리되지 않았다 — 다음 실행에서 신규로 보이고 상한 초과분이 조용히 유실된다", it.ID)
		}
	}
}

// 부트스트랩은 일회성 복구 수단이다. 끄고 나면 그 다음 폴부터는 평소처럼
// 발송해야 한다 — 이게 깨지면 캐시 유실 한 번에 봇이 영구히 조용해진다.
func TestBootstrapDoesNotPersistAcrossRuns(t *testing.T) {
	st := newTestStore(t)
	_ = st.Subscribe("100")
	notifier := &fakeNotifier{}

	src := &fakeSource{items: []core.Item{{ID: "old", Title: "old"}}}
	New(Config{
		Name: "test", Source: src, Formatter: fakeFormatter{},
		Notifier: notifier, Store: st, PollInterval: time.Hour,
		BootstrapMode: true,
	}).PollOnce(context.Background())
	if len(notifier.sent) != 0 {
		t.Fatalf("부트스트랩 회차에서 발송됐다: %v", notifier.sent)
	}

	// 부트스트랩을 끄고 새 글이 하나 들어온다.
	src.items = append(src.items, core.Item{ID: "fresh", Title: "fresh"})
	New(Config{
		Name: "test", Source: src, Formatter: fakeFormatter{},
		Notifier: notifier, Store: st, PollInterval: time.Hour,
	}).PollOnce(context.Background())

	if len(notifier.sent) != 1 {
		t.Fatalf("부트스트랩 다음 회차 발송 %d건, 새 글 1건만 나가야 한다 (0이면 봇이 영구히 조용해진 것)", len(notifier.sent))
	}
}
