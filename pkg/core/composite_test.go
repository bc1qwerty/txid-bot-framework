package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"testing"
)

type fakeNotifier struct {
	name string
	err  error
}

func (f *fakeNotifier) Name() string { return f.name }
func (f *fakeNotifier) Send(ctx context.Context, recipient string, msg Message) error {
	return f.err
}

// 부분 실패(한 채널만 영구 실패, 다른 채널 성공)면 Send 는 nil 을 유지하되
// OnPartialFailure 가 실패 채널의 이름·에러로 불려야 한다. 이 훅이 없으면
// ErrPermanentRecipient 가 Runner 에 닿을 길이 없다(부분 성공 = nil 반환).
func TestMultiNotifierPartialFailureHookFires(t *testing.T) {
	permErr := fmt.Errorf("bot was kicked: %w", ErrPermanentRecipient)
	mn := NewMultiNotifier(
		&fakeNotifier{name: "telegram", err: permErr},
		&fakeNotifier{name: "band"},
	)
	mn.Logger = log.New(io.Discard, "", 0)

	var gotName []string
	var gotErr []error
	mn.OnPartialFailure = func(name string, err error) {
		gotName = append(gotName, name)
		gotErr = append(gotErr, err)
	}

	if err := mn.Send(context.Background(), "123", Message{Text: "x"}); err != nil {
		t.Fatalf("부분 성공은 nil 을 반환해야 한다(중복 재발송 방지), got %v", err)
	}
	if len(gotName) != 1 || gotName[0] != "telegram" {
		t.Fatalf("훅 호출 = %v, want [telegram] 1회", gotName)
	}
	if !IsPermanentRecipient(gotErr[0]) {
		t.Fatalf("훅이 받은 에러에서 영구 판정이 사라졌다: %v", gotErr[0])
	}
}

// 전 채널 성공이면 훅은 불리지 않는다.
func TestMultiNotifierPartialFailureHookSilentOnSuccess(t *testing.T) {
	mn := NewMultiNotifier(
		&fakeNotifier{name: "telegram"},
		&fakeNotifier{name: "band"},
	)
	called := false
	mn.OnPartialFailure = func(name string, err error) { called = true }

	if err := mn.Send(context.Background(), "123", Message{Text: "x"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if called {
		t.Fatal("전 채널 성공인데 OnPartialFailure 가 불렸다")
	}
}

// 전 채널 실패면 Send 가 error 를 반환해 Runner 의 정규 경로(재시도·영구
// 판정)가 처리한다 — 훅까지 겹으로 불리면 이중 경보가 된다.
func TestMultiNotifierPartialFailureHookSilentOnTotalFailure(t *testing.T) {
	mn := NewMultiNotifier(
		&fakeNotifier{name: "telegram", err: errors.New("boom")},
		&fakeNotifier{name: "band", err: errors.New("boom")},
	)
	called := false
	mn.OnPartialFailure = func(name string, err error) { called = true }

	if err := mn.Send(context.Background(), "123", Message{Text: "x"}); err == nil {
		t.Fatal("전 채널 실패는 error 를 반환해야 한다")
	}
	if called {
		t.Fatal("전 채널 실패인데 OnPartialFailure 가 불렸다 — Runner 경로와 이중 경보")
	}
}

// 훅이 nil 이어도(기존 소비자) 부분 실패 경로가 그대로 동작한다.
func TestMultiNotifierPartialFailureNilHookSafe(t *testing.T) {
	mn := NewMultiNotifier(
		&fakeNotifier{name: "telegram", err: errors.New("transient")},
		&fakeNotifier{name: "band"},
	)
	mn.Logger = log.New(io.Discard, "", 0)
	if err := mn.Send(context.Background(), "123", Message{Text: "x"}); err != nil {
		t.Fatalf("nil 훅 + 부분 성공은 nil 이어야 한다, got %v", err)
	}
}

// 훅이 panic 해도 Send 는 살아남는다(프로세스 사망 방지) — panic 은 로그로
// 회수되고, 실패 채널이 여럿이면 앞 채널 훅의 panic 이 뒤 채널 통지를 막지 않는다.
func TestMultiNotifierPartialFailureHookPanicRecovered(t *testing.T) {
	mn := NewMultiNotifier(
		&fakeNotifier{name: "telegram", err: errors.New("boom-a")},
		&fakeNotifier{name: "sms", err: errors.New("boom-b")},
		&fakeNotifier{name: "band"},
	)
	var buf bytes.Buffer
	mn.Logger = log.New(&buf, "", 0)

	var calls []string
	mn.OnPartialFailure = func(name string, err error) {
		calls = append(calls, name)
		panic("consumer hook bug: " + name)
	}

	if err := mn.Send(context.Background(), "123", Message{Text: "x"}); err != nil {
		t.Fatalf("훅 panic 이 Send 실패로 번지면 안 된다, got %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("훅 호출 = %v, want 실패 채널 2곳 모두(첫 panic 이 다음 호출을 막았다)", calls)
	}
	if !strings.Contains(buf.String(), "OnPartialFailure hook panicked") {
		t.Fatalf("panic 회수 로그가 없다: %q", buf.String())
	}
}

// ⚠ 훅을 배선하지 않은 봇에서는 이 로그 한 줄이 부분 실패의 **유일한 신호**다.
// 이 함대의 원격 로그 감시(check-vps-errors)는 journald 를 `Error|error|FATAL|fatal|…`
// 로 훑으므로, 문구에 그 표지가 없으면 감시 밖으로 떨어진다 — 2026-09-16 에
// 예전 문구("… (n of m failed)")가 정확히 그 상태였다. 문구를 고칠 때 이 계약을 깨지 말 것.
func TestMultiNotifierPartialFailureDefaultLogIsGreppableAsError(t *testing.T) {
	var buf bytes.Buffer
	mn := NewMultiNotifier(
		&fakeNotifier{name: "telegram", err: errors.New("kicked")},
		&fakeNotifier{name: "band"},
	)
	mn.Logger = log.New(&buf, "", 0)
	// OnPartialFailure 를 일부러 배선하지 않는다 — 기본 경로를 본다.

	if err := mn.Send(context.Background(), "123", Message{Text: "x"}); err != nil {
		t.Fatalf("부분 성공은 nil 이어야 한다, got %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "partial failure") {
		t.Fatalf("부분 실패가 로그에 없다: %q", out)
	}
	if !strings.Contains(out, "error") {
		t.Fatalf("로그에 'error' 표지가 없어 check-vps-errors 가 못 잡는다: %q", out)
	}
}
