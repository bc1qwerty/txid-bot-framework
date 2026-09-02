package notify

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/bc1qwerty/txid-bot-framework/pkg/core"
)

// 429 는 이 fleet 에서 가장 흔한 발송 실패다(텔레그램 한도가 IP 단위 공유라
// 봇당 6/분). isTransient 목록에 빠져 있으면 rate limit 마다 알림이 사라진다.
func TestIsTransientCoversRateLimit(t *testing.T) {
	if !isTransient(errors.New("Too Many Requests: retry after 30")) {
		t.Error("429 를 일시적 오류로 안 봄 — rate limit 마다 알림이 유실된다")
	}
	// 반대 방향: 영구 오류를 재시도하면 잘못된 토큰으로 매번 시간을 버린다.
	if isTransient(errors.New("Unauthorized")) {
		t.Error("401 을 재시도 대상으로 봄")
	}
}

// retryWait 는 "다시 시도할지, 얼마나 기다릴지" 를 정한다. 429 는 텔레그램이
// 알려 준 retry_after 를 그대로 따라야 한다 — 임의 백오프로 밀어붙이면 한도를
// 더 깎아먹는다.
func TestRetryWait(t *testing.T) {
	tgErr := &tgbotapi.Error{
		Message:            "Too Many Requests: retry after 7",
		ResponseParameters: tgbotapi.ResponseParameters{RetryAfter: 7},
	}
	wait, again := retryWait(tgErr, 1)
	if !again {
		t.Fatal("429 인데 재시도 안 함")
	}
	if wait != 7*time.Second {
		t.Errorf("retry_after 를 안 따름: %v (7s 여야)", wait)
	}

	// 네트워크 오류는 지수적으로 물러난다.
	if w, again := retryWait(&net.OpError{Op: "dial", Err: errors.New("boom")}, 2); !again || w != 4*time.Second {
		t.Errorf("네트워크 오류 백오프가 틀림: %v %v", w, again)
	}

	// 영구 오류는 즉시 포기.
	if _, again := retryWait(errors.New("Bad Request: chat not found"), 1); again {
		t.Error("영구 오류를 재시도 대상으로 봄")
	}
}

// ── Discord ──────────────────────────────────────────────────────────────
// 웹훅은 상태 검사는 있었지만 재시도가 없었다. 429 한 번에 알림이 사라졌다.

func discordTo(t *testing.T, srv *httptest.Server) *DiscordNotifier {
	t.Helper()
	n := NewDiscord(srv.URL)
	n.http = srv.Client()
	return n
}

func TestDiscordRetriesRateLimit(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.Header().Set("Retry-After", "0.01")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := discordTo(t, srv).Send(context.Background(), "", core.Message{Text: "hi"}); err != nil {
		t.Fatalf("429 뒤 재시도로 성공해야 하는데 실패: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("시도 횟수 %d (2 여야 — 429 는 재시도 대상)", got)
	}
}

// 재시도 요청도 같은 본문을 실어야 한다. 이건 우리가 손으로 다시 감싸서가
// 아니라 http.NewRequestWithContext 가 *bytes.Reader 에 대해 req.GetBody 를
// 채워 주기 때문이다 — Transport 가 재전송 때 그걸로 본문을 되살린다.
// 그 보장이 깨지면(예: 본문을 io.Reader 로 바꿔 GetBody 가 nil 이 되면)
// 재시도가 **빈 본문**으로 나가므로, 그 회귀를 여기서 잡는다.
func TestDiscordResendsBodyOnRetry(t *testing.T) {
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 2048)
		n, _ := r.Body.Read(buf)
		bodies = append(bodies, string(buf[:n]))
		if len(bodies) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := discordTo(t, srv).Send(context.Background(), "", core.Message{Text: "본문유지"}); err != nil {
		t.Fatalf("5xx 뒤 재시도로 성공해야: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("시도 %d회 (2 여야)", len(bodies))
	}
	if !strings.Contains(bodies[1], "본문유지") {
		t.Errorf("재시도가 본문을 잃었다: %q", bodies[1])
	}
}

func TestDiscordDoesNotRetryClientError(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	if err := discordTo(t, srv).Send(context.Background(), "", core.Message{Text: "hi"}); err == nil {
		t.Fatal("400 인데 성공으로 보고함")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("400 을 %d회 시도 (1 이어야 — 페이로드가 틀린 것이니 반복해도 같다)", got)
	}
}

func TestDiscordGivesUpAfterMaxAttempts(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	err := discordTo(t, srv).Send(context.Background(), "", core.Message{Text: "hi"})
	if err == nil {
		t.Fatal("계속 5xx 인데 성공으로 보고함")
	}
	if got := atomic.LoadInt32(&hits); got != discordRetryMax {
		t.Errorf("시도 %d회 (%d 여야)", got, discordRetryMax)
	}
}
