package notify

import (
	"errors"
	"net"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/bc1qwerty/txid-bot-framework/pkg/core"
)

// ⚠이 판정이 넓어지면 멀쩡한 구독자가 조용히 끊긴다. 일시적 실패가 영구로
// 새는 것을 막는 것이 이 시험의 요점이다.
func TestIsPermanentRecipient(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"차단됨(403)", &tgbotapi.Error{Code: 403, Message: "Forbidden: bot was blocked by the user"}, true},
		{"계정 삭제(403)", &tgbotapi.Error{Code: 403, Message: "Forbidden: user is deactivated"}, true},
		{"chat not found(400)", &tgbotapi.Error{Code: 400, Message: "Bad Request: chat not found"}, true},
		{"타입 없이 문자열만", errors.New("Forbidden: bot was blocked by the user"), true},

		// 아래는 전부 재시도해야 하는 것들이다.
		{"429", &tgbotapi.Error{Code: 429, Message: "Too Many Requests: retry after 5"}, false},
		{"502", errors.New("telegram: Bad Gateway"), false},
		{"네트워크", &net.OpError{Op: "read", Err: errors.New("connection reset by peer")}, false},
		{"페이로드 오류(400)", &tgbotapi.Error{Code: 400, Message: "Bad Request: message text is empty"}, false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isPermanentRecipient(c.err); got != c.want {
				t.Fatalf("isPermanentRecipient(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// ⚠scrub 은 errors.New 로 사슬을 끊는다. 판정이 scrub 뒤로 밀리면 sentinel 이
// 붙지 않아 Runner 가 영구 실패를 못 알아본다 — 순서 자체가 계약이다.
// 그리고 토큰은 그 와중에도 지워져 있어야 한다.
func TestFinishWrapsPermanentAndScrubsToken(t *testing.T) {
	const token = "123456:SECRET-TOKEN"
	tg := &Telegram{api: &tgbotapi.BotAPI{Token: token}}

	err := tg.finish(errors.New("Post \"https://api.telegram.org/bot" + token +
		"/sendMessage\": Forbidden: bot was blocked by the user"))

	if !core.IsPermanentRecipient(err) {
		t.Fatalf("영구 실패로 감싸지지 않았다: %v", err)
	}
	if got := err.Error(); contains(got, token) {
		t.Fatalf("토큰이 그대로 남았다: %s", got)
	}
}

func TestFinishLeavesTransientAlone(t *testing.T) {
	tg := &Telegram{api: &tgbotapi.BotAPI{Token: "t"}}
	err := tg.finish(errors.New("Too Many Requests: retry after 5"))
	if core.IsPermanentRecipient(err) {
		t.Fatalf("일시 실패를 영구로 판정했다: %v", err)
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
