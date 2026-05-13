// Package notify implements message delivery channels.
package notify

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/bc1qwerty/txid-bot-framework/pkg/core"
)

// Telegram delivers messages via the Telegram Bot API.
type Telegram struct {
	api *tgbotapi.BotAPI
}

// NewTelegram creates a new Telegram notifier.
func NewTelegram(token string) (*Telegram, error) {
	api, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, fmt.Errorf("telegram bot api: %w", err)
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

	if msg.ImageURL != "" {
		var photo tgbotapi.PhotoConfig
		if channelUsername != "" {
			photo = tgbotapi.NewPhotoToChannel(channelUsername, tgbotapi.FileURL(msg.ImageURL))
		} else {
			photo = tgbotapi.NewPhoto(chatID, tgbotapi.FileURL(msg.ImageURL))
		}
		photo.Caption = msg.Text
		photo.ParseMode = msg.ParseMode
		if hasKeyboard {
			photo.ReplyMarkup = keyboard
		}
		if _, err := t.api.Send(photo); err == nil {
			return nil
		}
		// Fall through to text if image send failed
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
	return err
}
