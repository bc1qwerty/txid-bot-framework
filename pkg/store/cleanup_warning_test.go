package store

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"
)

// fw-05: Cleanup 이 bot_send_failure 의 오래된 행을 지울 때는 경고를 남겨야
// 한다 — 그 행은 「발송 실패 후 소스 윈도 밖으로 빠져 재시도 없이 유실된
// 아이템」의 유일한 흔적이라, 무음 삭제는 증거 인멸이 된다.
func TestCleanupWarnsBeforePurgingSendFailures(t *testing.T) {
	st := openTestStore(t)

	for _, id := range []string{"lost-1", "lost-2"} {
		if _, err := st.RecordSendFailure("g2b", id, "429 too many requests"); err != nil {
			t.Fatalf("record failure %s: %v", id, err)
		}
	}
	// 재시도가 아직 살아 있는(최근) 행 — 지워지면 안 된다.
	if _, err := st.RecordSendFailure("g2b", "active-1", "429"); err != nil {
		t.Fatalf("record failure active-1: %v", err)
	}
	// 이틀 전으로 백데이트해 cutoff(1일) 에 걸리게 한다.
	if _, err := st.DB().Exec(
		`UPDATE bot_send_failure SET updated_at = unixepoch() - 172800 WHERE item_id LIKE 'lost-%'`); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	if err := st.Cleanup(24 * time.Hour); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "WARNING") || !strings.Contains(out, "2 stale bot_send_failure") {
		t.Fatalf("경고에 행 수가 없다: %q", out)
	}
	if !strings.Contains(out, "lost-1") || !strings.Contains(out, "lost-2") {
		t.Fatalf("경고에 item id 표본이 없다: %q", out)
	}

	// 실제 삭제도 일어났는지 — 오래된 행만.
	if n, err := st.SendFailureAttempts("g2b", "lost-1"); err != nil || n != 0 {
		t.Fatalf("lost-1 이 안 지워졌다: attempts=%d err=%v", n, err)
	}
	if n, err := st.SendFailureAttempts("g2b", "active-1"); err != nil || n != 1 {
		t.Fatalf("active-1 은 남아야 한다: attempts=%d err=%v", n, err)
	}
}

// 지울 것이 없으면 경고도 없어야 한다 — 매일 도는 Cleanup 이 상시 소음을
// 내면 진짜 경고가 묻힌다.
func TestCleanupSilentWhenNoStaleSendFailures(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.RecordSendFailure("g2b", "active-1", "429"); err != nil {
		t.Fatalf("record failure: %v", err)
	}

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	if err := st.Cleanup(24 * time.Hour); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if strings.Contains(buf.String(), "bot_send_failure") {
		t.Fatalf("지울 것이 없는데 경고가 났다: %q", buf.String())
	}
}
