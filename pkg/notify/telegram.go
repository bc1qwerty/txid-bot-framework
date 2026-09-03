// Package notify implements message delivery channels.
package notify

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/bc1qwerty/txid-bot-framework/pkg/core"

	"github.com/bc1qwerty/txid-bot-framework/pkg/logsafe"
)

// ⚠tgbotapi 는 오류를 찍을 때 요청 URL 을 통째로 담는데, 텔레그램은 **봇 토큰을 URL
// 경로에 넣는다**. 그대로 두면 journald 에 실토큰이 평문으로 남는다(2026-09-03 에
// VPS 에서 30일 15건 발견). 로거는 패키지 전역이라 여기서 한 번 갈아 두면 이 프로세스의
// 모든 tgbotapi 로그에 적용된다. 암묵적 init 을 쓰는 이유는 이것이 **선택 사항이 아니라
// 무조건 지켜야 하는 불변식**이기 때문이다 — 부르는 것을 잊으면 토큰이 샌다.
func init() { logsafe.Install() }

// telegramCaptionLimit is the Bot API's max caption length for a photo,
// in UTF-16 code units. Counting runes is a safe under-approximation
// (a rune is 1 or 2 units) except for emoji-heavy text, so we stay
// comfortably inside the real limit.
const telegramCaptionLimit = 1000

// Telegram delivers messages via the Telegram Bot API.
type Telegram struct {
	api *tgbotapi.BotAPI
}

// 토큰은 URL 안에 들어간다(`/bot<토큰>/<method>`). tgbotapi 는 네트워크 오류를
// 그대로 돌려주고, net/http 의 *url.Error 는 그 URL 을 담는다. 그래서 발송 실패를
// 로그에 찍는 순간 토큰이 평문으로 남는다.
//
// GitHub Actions 는 secrets 를 자동으로 가려 주지만, 이 패키지를 쓰는 봇 중에는
// mac launchd 로 도는 것도 있고 그쪽 로그는 아무도 안 가려 준다. 같은 실수를
// txiduk-bot 이 실제로 32줄 남긴 뒤에 여기도 막았다.
//
// 오류를 삼키지는 않는다 — 토큰만 지우고 사유는 그대로 남긴다.
func scrubToken(token string, err error) error {
	if err == nil || token == "" {
		return err
	}
	msg := strings.ReplaceAll(err.Error(), token, "***")
	if msg == err.Error() {
		return err
	}
	return errors.New(msg)
}

func (t *Telegram) scrub(err error) error {
	if t == nil || t.api == nil {
		return err
	}
	return scrubToken(t.api.Token, err)
}

// NewTelegram creates a new Telegram notifier.
//
// ⚠ NewBotAPI 는 곧바로 getMe 를 친다. 그 한 번이 네트워크 사정으로 실패하면
// 봇이 통째로 죽는데, 호출부 대부분이 log.Fatalf 라 그 실행은 그대로 끝난다.
// 2026-08-30 실측: safety_alarm_bot 은 정상 완료 5,258회에 대해 네트워크 오류가
// 2,202회였다 — **약 30% 실행이 여기서 죽고 있었다.** 30분 뒤 다음 실행이
// 회복하므로 결과물로는 잘 안 드러나고, launchd 종료코드에만 남는다.
//
// 그래서 **일시적 오류만** 짧게 재시도한다. 토큰이 틀린 경우(401 Unauthorized)는
// 몇 번을 쳐도 같으므로 즉시 포기한다 — 안 그러면 잘못된 토큰으로 기동할 때마다
// 쓸데없이 3초를 버린다.
func NewTelegram(token string) (*Telegram, error) {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
		var api *tgbotapi.BotAPI
		api, err = tgbotapi.NewBotAPI(token)
		if err == nil {
			return &Telegram{api: api}, nil
		}
		if !isTransient(err) {
			break
		}
	}
	return nil, fmt.Errorf("telegram bot api: %w", scrubToken(token, err))
}

// isTransient 는 재시도할 값어치가 있는 오류인지 본다.
// ⚠ 문자열 판정을 쓰는 이유: tgbotapi 는 HTTP 오류를 자체 타입으로 감싸지 않고
// net 계열 오류를 그대로 올려보내기도 해서 errors.As 로 일관되게 못 가른다.
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	s := err.Error()
	for _, m := range []string{
		"connection reset", "read tcp", "i/o timeout", "EOF",
		"connection refused", "no such host", "TLS handshake",
		"Bad Gateway", "Service Unavailable", "Gateway Timeout",
		// ⚠ 429 는 가장 흔한 일시적 실패인데 빠져 있었다. 이 fleet 은 텔레그램
		// 한도가 **IP 단위 공유**라(봇당 6/분) 실제로 자주 걸리고, api.txid.uk 가
		// 같은 한도로 7일간 525건을 잃은 적이 있다(2026-08-21).
		"Too Many Requests",
	} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// sendRetryMax 는 총 시도 횟수(최초 1 + 재시도 2)다.
// ⚠ 재시도를 늘리는 건 **호출이 텔레그램 쪽에서 멱등할 때만** 안전하다 —
// sendMessage·sendPhoto·sendDocument 는 멱등이다(safety_alarm_bot 이 같은 판단으로
// 이미 이 정책을 쓰고 있다). 새 메서드를 여기 태우기 전에 그것부터 확인할 것.
const sendRetryMax = 3

// retryWait 는 이 오류를 다시 시도할지, 얼마나 기다릴지 답한다.
// 429 는 텔레그램이 parameters.retry_after 로 대기 시간을 알려 주므로 그걸 따른다
// — 임의 백오프로 밀어붙이면 한도를 더 깎아먹는다.
func retryWait(err error, attempt int) (time.Duration, bool) {
	var tgErr *tgbotapi.Error
	if errors.As(err, &tgErr) && tgErr.RetryAfter > 0 {
		return time.Duration(tgErr.RetryAfter) * time.Second, true
	}
	if isTransient(err) {
		return time.Duration(attempt*2) * time.Second, true
	}
	return 0, false
}

// send 는 일시적 실패를 짧게 재시도한다.
// ⚠ 그전까지 재시도는 **NewTelegram(기동)에만** 있었고 정작 메시지 발송에는
// 없었다. 즉 봇은 살아서 뜨는데 알림 한 건이 429·순간 네트워크 오류로 조용히
// 사라질 수 있었다. 4xx(429 제외)는 우리 페이로드가 틀린 것이므로 즉시 포기한다.
func (t *Telegram) send(c tgbotapi.Chattable, kind string) error {
	var err error
	for attempt := 1; attempt <= sendRetryMax; attempt++ {
		if _, err = t.api.Send(c); err == nil {
			return nil
		}
		wait, again := retryWait(err, attempt)
		if !again || attempt == sendRetryMax {
			break
		}
		log.Printf("[telegram] %s 재시도 %d/%d (%v 뒤): %v", kind, attempt+1, sendRetryMax, wait, t.scrub(err))
		time.Sleep(wait)
	}
	return err
}

// API returns the underlying bot API for custom handlers.
func (t *Telegram) API() *tgbotapi.BotAPI {
	return t.api
}

// Name returns the channel identifier.
func (t *Telegram) Name() string {
	return "telegram"
}

// Send delivers a message to a chat ID or channel username.
// Recipient may be a numeric chat ID (e.g. "-1001234567890") or a public
// channel username starting with "@" (e.g. "@safety_alarm_korea").
func (t *Telegram) Send(ctx context.Context, recipient string, msg core.Message) error {
	var chatID int64
	var channelUsername string
	if strings.HasPrefix(recipient, "@") {
		channelUsername = recipient
	} else {
		id, err := strconv.ParseInt(recipient, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid chat id %q: %w", recipient, err)
		}
		chatID = id
	}

	var keyboard tgbotapi.InlineKeyboardMarkup
	hasKeyboard := false
	if len(msg.Buttons) > 0 {
		rows := make([][]tgbotapi.InlineKeyboardButton, 0, len(msg.Buttons))
		for _, row := range msg.Buttons {
			btns := make([]tgbotapi.InlineKeyboardButton, 0, len(row))
			for _, b := range row {
				if b.URL != "" {
					btns = append(btns, tgbotapi.NewInlineKeyboardButtonURL(b.Text, b.URL))
				} else if b.Data != "" {
					btns = append(btns, tgbotapi.NewInlineKeyboardButtonData(b.Text, b.Data))
				}
			}
			if len(btns) > 0 {
				rows = append(rows, btns)
			}
		}
		if len(rows) > 0 {
			keyboard = tgbotapi.NewInlineKeyboardMarkup(rows...)
			hasKeyboard = true
		}
	}

	// A document is the material itself (a KOSHA OPS sheet is a PDF), so it
	// outranks any preview image. Sent as a document rather than a photo on
	// purpose: Telegram re-encodes photos, which shreds small type, and a
	// photo cannot be saved back out as the original PDF.
	docSent := false
	if len(msg.FileData) > 0 {
		name := msg.FileName
		if name == "" {
			name = "file"
		}
		blob := tgbotapi.FileBytes{Name: name, Bytes: msg.FileData}
		// v5 has NewPhotoToChannel but no document equivalent; the channel
		// username goes on BaseChat, which the API prefers over ChatID.
		doc := tgbotapi.NewDocument(chatID, blob)
		doc.ChannelUsername = channelUsername
		// Same caption rule as photos: Telegram rejects the whole send when
		// the caption overflows, so drop the caption instead of the file and
		// let the text message below carry the body.
		captionFits := len([]rune(msg.Text)) <= telegramCaptionLimit
		if captionFits {
			doc.Caption = msg.Text
			doc.ParseMode = msg.ParseMode
			if hasKeyboard {
				doc.ReplyMarkup = keyboard
			}
		}
		if err := t.send(doc, "document"); err == nil {
			docSent = true
			if captionFits {
				return nil
			}
			// Caption did not fit: fall through for the text body only. The
			// image below must stay skipped or the post arrives twice.
		} else {
			// Say why rather than silently degrading: alerts would keep
			// arriving and hide a broken upload path.
			log.Printf("[telegram] document send failed for %s (%s, %d bytes), falling back: %v",
				recipient, name, len(msg.FileData), t.scrub(err))
		}
	}

	// ImageData (an upload) wins over ImageURL (a fetch-by-Telegram),
	// because a source that inlines its image has no URL to offer.
	var file tgbotapi.RequestFileData
	switch {
	case docSent:
		// already delivered as a document; only the text body is left
	case len(msg.ImageData) > 0:
		name := msg.ImageName
		if name == "" {
			name = "image.jpg"
		}
		file = tgbotapi.FileBytes{Name: name, Bytes: msg.ImageData}
	case msg.ImageURL != "":
		file = tgbotapi.FileURL(msg.ImageURL)
	}

	if file != nil {
		var photo tgbotapi.PhotoConfig
		if channelUsername != "" {
			photo = tgbotapi.NewPhotoToChannel(channelUsername, file)
		} else {
			photo = tgbotapi.NewPhoto(chatID, file)
		}
		// Telegram caps photo captions at 1024 chars (vs 4096 for a plain
		// message) and rejects the whole send when it overflows. Drop the
		// caption rather than lose the photo; the text message below is
		// suppressed only on success, so an over-long body still needs its
		// own message.
		caption, captionFits := msg.Text, len([]rune(msg.Text)) <= telegramCaptionLimit
		if captionFits {
			photo.Caption = caption
			photo.ParseMode = msg.ParseMode
			if hasKeyboard {
				photo.ReplyMarkup = keyboard
			}
		}
		if err := t.send(photo, "photo"); err == nil {
			if captionFits {
				return nil
			}
		} else {
			// Fall through to text, but say why: a silent downgrade would
			// hide a broken image pipeline behind still-arriving alerts.
			log.Printf("[telegram] photo send failed for %s, falling back to text: %v", recipient, t.scrub(err))
		}
	}

	var text tgbotapi.MessageConfig
	if channelUsername != "" {
		text = tgbotapi.NewMessageToChannel(channelUsername, msg.Text)
	} else {
		text = tgbotapi.NewMessage(chatID, msg.Text)
	}
	text.ParseMode = msg.ParseMode
	text.DisableWebPagePreview = false
	if hasKeyboard {
		text.ReplyMarkup = keyboard
	}
	return t.scrub(t.send(text, "message"))
}
