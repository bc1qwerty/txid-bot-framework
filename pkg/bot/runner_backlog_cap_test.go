package bot

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/bc1qwerty/txid-bot-framework/pkg/core"
)

// 백로그 상한 초과분은 **발송 없이 seen 처리돼 영구 유실**된다 — 다시는 후보가
// 되지 않으므로 사용자가 받았어야 할 알림이 통째로 사라진다.
//
// 그런데 오래도록 흔적이 로그 한 줄뿐이었고, 그 줄에는 error 계열 낱말이 없어
// 함대의 원격 로그 감시(check-vps-errors 의 `Error|error|FATAL|...`)에 걸리지
// 않았다. best-archive 는 2026-09-12~14 에 16회 중 5회가 상한에 걸려 17건을
// 잃었는데 감사 때 사람이 로그를 눈으로 읽어서야 알았다.
//
// 같은 파일의 «발송 포기» 경로는 이미 «seen 처리하되 로그가 아니라 OnError 로
// 승격시킨다» 는 원칙을 세워 뒀다. 영구 폐기라는 점이 똑같은 이 자리만 빠져
// 있었다. 두 경로를 다 고정한다:
//
//  1. OnError 가 불린다 — 봇의 허브 알림 경로가 여기 붙어 있다.
//  2. 로그 줄이 원격 감시 패턴에 걸린다 — OnError 를 배선하지 않은 봇
//     (social-feed 가 그렇다)에서는 이 줄이 유일한 흔적이다.
func TestBacklogCapDropIsSurfacedNotSilent(t *testing.T) {
	st := newTestStore(t)
	if err := st.Subscribe("100"); err != nil {
		t.Fatal(err)
	}
	notifier := &fakeNotifier{}

	items := make([]core.Item, 0, 8)
	for _, id := range []string{"i1", "i2", "i3", "i4", "i5", "i6", "i7", "i8"} {
		items = append(items, core.Item{ID: id, Title: id})
	}
	src := &fakeSource{items: items}

	var errs []error
	var logBuf bytes.Buffer

	r := New(Config{
		Name: "test", Source: src, Formatter: fakeFormatter{},
		Notifier: notifier, Store: st, PollInterval: time.Hour,
		MaxItemsPerPoll: 3,
		OnError:         func(e error) { errs = append(errs, e) },
	})
	r.log = log.New(&logBuf, "", 0)
	r.PollOnce(context.Background())

	// 전제: 상한이 실제로 걸렸다(3건 발송, 5건 폐기).
	if len(notifier.sent) != 3 {
		t.Fatalf("상한 3인데 %d건 발송됐다 — 이 시험의 전제가 깨졌다", len(notifier.sent))
	}

	// ① 사람 경보 경로
	if len(errs) != 1 {
		t.Fatalf("상한으로 5건을 영구 폐기하고 OnError 를 %d회 불렀다 — "+
			"사용자가 받았어야 할 알림이 사라졌는데 사람이 알 방법이 없다", len(errs))
	}
	if !strings.Contains(errs[0].Error(), "5") {
		t.Fatalf("OnError 문구에 폐기 건수가 없다: %v — 몇 건을 잃었는지가 이 경보의 핵심이다", errs[0])
	}

	// ② 원격 로그 감시와의 계약. 문구를 바꿀 때 이 시험이 같이 깨져야 한다.
	logged := logBuf.String()
	if !strings.Contains(logged, "backlog cap") {
		t.Fatalf("상한 폐기가 로그에 남지 않았다:\n%s", logged)
	}
	capLine := ""
	for _, ln := range strings.Split(logged, "\n") {
		if strings.Contains(ln, "backlog cap") {
			capLine = ln
			break
		}
	}
	// check-vps-errors 의 기본 패턴에서 이 줄에 해당하는 부분만 뽑아 쓴다.
	if !strings.Contains(capLine, "error") && !strings.Contains(capLine, "Error") {
		t.Fatalf("상한 폐기 로그에 error 표지가 없다 — 원격 로그 감시가 "+
			"`Error|error|FATAL|...` 로 훑으므로 이 줄은 걸리지 않는다. OnError 를 "+
			"배선하지 않은 봇에서는 이것이 유일한 흔적이다:\n  %s", capLine)
	}

	// ③ 폐기분이 정말 영구인지(다음 폴에 안 돌아온다) — 경보의 심각도 근거다.
	r.PollOnce(context.Background())
	if len(notifier.sent) != 3 {
		t.Fatalf("두 번째 폴에서 %d건이 더 나갔다 — 폐기가 영구가 아니라면 "+
			"경보 문구(영구 폐기)가 거짓이다", len(notifier.sent)-3)
	}
}
