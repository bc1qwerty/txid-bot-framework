package notify

import (
	"errors"
	"net"
	"testing"
)

// ⚠ 이 판정이 틀리면 둘 중 하나가 된다: 일시적 오류를 안 재시도해서 30% 실행이
// 계속 죽거나(고치기 전 상태), 인증 오류를 재시도해서 매번 3초를 버리거나.
func TestIsTransient(t *testing.T) {
	transient := []error{
		errors.New(`Post "https://api.telegram.org/botX/getMe": read tcp 192.168.0.10:49986->149.154.166.110:443: read: connection reset by peer`),
		errors.New("dial tcp: i/o timeout"),
		errors.New("unexpected EOF"),
		errors.New("connection refused"),
		errors.New("dial tcp: lookup api.telegram.org: no such host"),
		errors.New("net/http: TLS handshake timeout"),
		errors.New("502 Bad Gateway"),
		&net.OpError{Op: "dial", Err: errors.New("boom")},
	}
	for _, e := range transient {
		if !isTransient(e) {
			t.Errorf("일시적으로 봐야 하는데 아님: %v", e)
		}
	}
	permanent := []error{
		nil,
		errors.New("Not Found"),
		errors.New("Unauthorized"),
		errors.New("401 Unauthorized: bot token is invalid"),
		errors.New("Forbidden: bot was blocked by the user"),
	}
	for _, e := range permanent {
		if isTransient(e) {
			t.Errorf("영구 오류인데 재시도 대상으로 봄: %v", e)
		}
	}
}
