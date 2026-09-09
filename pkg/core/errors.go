package core

import "errors"

// ErrPermanentRecipient 는 "이 수신자에게는 앞으로도 못 보낸다" 를 뜻한다.
//
// 채널 구현(notify.Telegram 등)이 텔레그램의 403 계열 응답
// (`bot was blocked by the user`·`user is deactivated`·`chat not found` 등)을
// 이 sentinel 로 감싸 올리면, Runner 는 그것을 일시적 실패와 구별해
// **재시도 대신 구독 비활성화**로 처리한다.
//
// ⚠이 구별이 없으면 차단당한 구독 하나가 영구 소음이 된다. 실제로 nara-bot 에서
// `send error … Forbidden: bot was blocked by the user` 가 새 공고가 올 때마다
// 반복됐다(2026-09-09). 재시도 상한(maxSendAttempts)은 **아이템 단위**라, 아이템이
// 새로 올 때마다 카운터가 처음부터 다시 시작해 상한이 아무것도 막지 못한다.
//
// 판정은 보수적이어야 한다 — 일시적 실패를 영구로 잘못 읽으면 멀쩡한 구독자가
// 조용히 끊긴다. 그래서 429·5xx·네트워크 오류는 절대 여기 들어오지 않는다.
var ErrPermanentRecipient = errors.New("permanent recipient failure")

// IsPermanentRecipient 는 err 사슬에 ErrPermanentRecipient 가 있는지 본다.
func IsPermanentRecipient(err error) bool {
	return errors.Is(err, ErrPermanentRecipient)
}
