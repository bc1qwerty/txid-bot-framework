package bot

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/bc1qwerty/txid-bot-framework/pkg/core"
	"github.com/bc1qwerty/txid-bot-framework/pkg/store"
)

// dedup 조회가 실패했을 때의 처리.
//
// ⚠ `if err != nil || sent { continue }` 였다. "조회 실패"와 "이미 보냈다"가 같은
//   결과로 처리돼, 아이템이 루프 끝에서 seen 처리되고 **그 구독자는 그 알림을 영영
//   못 받았다.** 로그조차 남지 않아 흔적도 없었다. framework 를 쓰는 모든 봇에
//   공통이었고, 같은 모양의 결함을 realestate-alert-bot 의 레거시 poller 에서
//   2026-09-10 에 고치면서 이쪽에도 남아 있는 것을 확인했다.

// 파일 기반 스토어 — 바깥에서 같은 DB 를 열어 테이블을 깨뜨릴 수 있어야 한다
// (`:memory:` 는 연결마다 별개 DB 라 불가능하다).
func newFileStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bot.db")
	st, err := store.Open(path, "test-bot")
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, path
}

func TestIsSentLookupErrorDefersInsteadOfDropping(t *testing.T) {
	st, path := newFileStore(t)
	if err := st.Subscribe("100"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	src := &fakeSource{items: []core.Item{{ID: "x", Title: "Xray"}}}
	n := &fakeNotifier{}
	var errs []error
	r := New(Config{
		Name: "test", Source: src, Formatter: fakeFormatter{},
		Notifier: n, Store: st, PollInterval: time.Hour,
		OnError: func(e error) { errs = append(errs, e) },
	})

	// bot_sent 만 깨뜨려 IsSent 를 실패시킨다(다른 경로는 살려 둔다).
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec("DROP TABLE bot_sent"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	_ = raw.Close()

	r.PollOnce(context.Background())

	if len(n.sent) != 0 {
		t.Fatalf("조회가 실패했는데 발송했다: %v", n.sent)
	}
	// 판단하지 못한 아이템은 seen 처리하면 안 된다 — 하면 영영 사라진다.
	seen, err := st.IsSeen("fake", "x")
	if err != nil {
		t.Fatalf("IsSeen: %v", err)
	}
	if seen {
		t.Fatal("판단하지 못한 아이템이 seen 처리됐다 — 그 알림은 영영 전달되지 않는다")
	}
}

// 조회가 성공하고 "이미 보냈다"면 조용히 건너뛰는 것이 맞다 — 위 변경이 그 길을
// 막지 않았는지 고정한다.
func TestAlreadySentStillSkipsQuietly(t *testing.T) {
	st, _ := newFileStore(t)
	if err := st.Subscribe("100"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	src := &fakeSource{items: []core.Item{{ID: "x", Title: "Xray"}}}
	n := &fakeNotifier{}
	r := New(Config{
		Name: "test", Source: src, Formatter: fakeFormatter{},
		Notifier: n, Store: st, PollInterval: time.Hour,
	})

	r.PollOnce(context.Background())
	r.PollOnce(context.Background())

	if len(n.sent) != 1 {
		t.Fatalf("같은 알림이 %d회 발송됐다", len(n.sent))
	}
}
