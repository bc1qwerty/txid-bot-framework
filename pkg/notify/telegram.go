// Package notify implements message delivery channels.
package notify

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/bc1qwerty/txid-bot-framework/pkg/core"
)

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
func NewTelegram(token string) (*Telegram, error) {
	api, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, fmt.Errorf("telegram bot api: %w", scrubToken(token, err))
	}
	return &Telegram{api: api}, nil
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
		if _, err := t.api.Send(doc); err == nil {
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
		if _, err := t.api.Send(photo); err == nil {
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
	_, err := t.api.Send(text)
	return t.scrub(err)
}
