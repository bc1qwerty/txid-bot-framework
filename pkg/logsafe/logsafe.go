// Package logsafe 는 로그에 실려 나가는 텔레그램 봇 토큰을 가린다.
//
// 왜 필요한가 (2026-09-03):
// 텔레그램은 봇 토큰을 **URL 경로**에 넣는다(`api.telegram.org/bot<token>/method`).
// 그래서 Go 의 http 오류는 URL 을 통째로 담고, tgbotapi 는 그 err 를 그대로 찍는다
// (`bot.go:445  log.Println(err)`). 결과로 VPS journald 에 실토큰이 평문으로 남아 있었다
// — 30일 창에 15건, 봇 3개분.
//
//	Post "https://api.telegram.org/bot<실토큰>/getUpdates": read tcp ...: connection reset
//
// 노출 자체는 VPS 로컬(adm 그룹)이지만, 로그는 백업·복사·붙여넣기로 쉽게 퍼진다.
// 2026-08-31 에 토큰 하나가 공개 리포로 나가 **봇이 탈취되고 이름·설명이 스팸으로
// 교체된** 전례가 있으므로 경로 자체를 막는다.
//
// ⚠tgbotapi 의 Debug 모드는 `Endpoint: %s, params: %v` 로 요청/응답을 통째로 찍는다.
// 이 로거는 그쪽도 같이 덮는다.
package logsafe

import (
	"fmt"
	stdlog "log"
	"os"
	"regexp"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// 텔레그램 토큰 형태: 숫자 봇ID + ':' + 35자 안팎의 시크릿.
// ⚠`bot` 접두어를 요구하지 않는다 — URL 밖에서 토큰만 찍히는 경우도 덮기 위해서다.
// ⚠**앞에 `\b` 를 쓰면 안 된다.** 실제 로그의 형태는 `.../bot<숫자ID>:<시크릿>` 이라
//
//	숫자 앞 글자가 `t`(단어문자)여서 단어 경계가 성립하지 않는다 — 그러면 정작 가장
//	흔한 노출 형태를 놓친다(실제로 그렇게 짰다가 시험에서 잡혔다). Go 에는 lookbehind
//	가 없으므로 경계 대신 **시크릿 길이 하한**으로 오탐을 막는다.
//
// ⚠시크릿 30자 하한이 그 방어선이다: `12:34`(시각)·`16:9`(비율)·`:51922`(포트)처럼
//
//	콜론이 들어가는 평범한 로그를 가리지 않는다.
var tokenRe = regexp.MustCompile(`(\d{6,})(:[A-Za-z0-9_-]{30,})`)

// Mask 는 문자열에서 텔레그램 봇 토큰의 시크릿부를 가린다.
// 봇 ID 는 남긴다 — 어느 봇인지는 알아야 진단이 된다(그 자체는 비밀이 아니다).
func Mask(s string) string {
	return tokenRe.ReplaceAllString(s, "$1:REDACTED")
}

type maskingLogger struct{ out *stdlog.Logger }

func (l maskingLogger) Println(v ...interface{}) {
	// ⚠Sprintln 이 끝에 개행을 붙이므로 Print 로 내보낸다. Println 을 쓰면 빈 줄이 생긴다.
	l.out.Print(Mask(fmt.Sprintln(v...)))
}

func (l maskingLogger) Printf(format string, v ...interface{}) {
	s := Mask(fmt.Sprintf(format, v...))
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	l.out.Print(s)
}

// Install 은 tgbotapi 의 패키지 로거를 마스킹 로거로 교체한다.
//
// ⚠tgbotapi 의 로거는 **패키지 전역 변수**다(`log.go` 의 `var log BotLogger`).
// 따라서 한 번만 부르면 그 프로세스의 모든 tgbotapi 로그에 적용된다.
func Install() {
	_ = tgbotapi.SetLogger(maskingLogger{out: stdlog.New(os.Stderr, "", stdlog.LstdFlags)})
}
