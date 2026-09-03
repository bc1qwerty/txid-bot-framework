package logsafe

import (
	"strings"
	"testing"
)

// 실제로 journald 에 남아 있던 형태. 토큰은 **가짜**이며, 그마저도 리터럴로 두지 않고
// 런타임에 조립한다 — 시크릿 스캐너가 이 파일을 토큰 유출로 잡아 커밋을 막기 때문이다.
// (처음에 진짜 값을 붙여 넣었다가 gitleaks 훅이 잡았다. 훅을 우회하지 말 것.)
var (
	fakeBotID  = "1234567890"
	fakeSecret = "AA" + strings.Repeat("b", 15) + "_" + strings.Repeat("C", 17)
	fakeToken  = fakeBotID + ":" + fakeSecret
	realShape  = `Post "https://api.telegram.org/bot` + fakeToken +
		`/getUpdates": read tcp 10.0.0.148:51922->149.154.166.110:443: read: connection reset by peer`
)

func TestMask_가린다(t *testing.T) {
	got := Mask(realShape)
	if strings.Contains(got, fakeSecret) {
		t.Fatalf("토큰이 그대로 남았다: %s", got)
	}
	if !strings.Contains(got, fakeBotID+":REDACTED") {
		t.Fatalf("봇 ID 는 남겨야 어느 봇인지 안다: %s", got)
	}
	// ⚠주소·포트는 진단에 필요하다. 과하게 가리면 이 로그가 쓸모없어진다.
	if !strings.Contains(got, "10.0.0.148:51922") || !strings.Contains(got, "149.154.166.110:443") {
		t.Fatalf("IP·포트까지 가려 버렸다: %s", got)
	}
}

func TestMask_오탐하지_않는다(t *testing.T) {
	// 이 패턴들을 가리면 평범한 로그가 읽을 수 없게 된다.
	for _, s := range []string{
		"2026/09/03 12:29:24 Polling complete",
		"read tcp 10.0.0.148:51922->149.154.166.110:443",
		"Endpoint: getMe, took 273ms",
		"ratio 16:9 scaled",
		"sha256:abc123", // 콜론 뒤가 길어도 앞이 숫자가 아니면 대상 아님
		"1234:short",    // 시크릿이 짧으면 토큰이 아니다
	} {
		if got := Mask(s); got != s {
			t.Errorf("오탐: %q → %q", s, got)
		}
	}
}

func TestMask_URL_밖의_토큰도_가린다(t *testing.T) {
	s := "TELEGRAM_TOKEN=" + fakeToken + " loaded"
	if strings.Contains(Mask(s), fakeSecret) {
		t.Fatalf("URL 밖에서도 가려야 한다: %s", Mask(s))
	}
}

// 로거가 tgbotapi 의 두 메서드를 만족하고 마스킹을 실제로 적용하는지.
func TestLogger_마스킹_적용(t *testing.T) {
	var sb strings.Builder
	l := maskingLogger{out: newTestLogger(&sb)}
	l.Println("err:", realShape)
	l.Printf("endpoint=%s", realShape)
	out := sb.String()
	if strings.Contains(out, fakeSecret) {
		t.Fatalf("로거를 통과했는데 토큰이 남았다: %s", out)
	}
	if n := strings.Count(out, fakeBotID+":REDACTED"); n != 2 {
		t.Fatalf("Println·Printf 둘 다 마스킹돼야 한다 (실제 %d회): %s", n, out)
	}
	// ⚠Sprintln 의 개행 때문에 빈 줄이 생기지 않아야 한다.
	if strings.Contains(out, "\n\n") {
		t.Fatalf("빈 줄이 생겼다: %q", out)
	}
}
