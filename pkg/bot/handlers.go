package bot

import (
	"context"
	"strconv"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/bc1qwerty/txid-bot-framework/pkg/notify"
	"github.com/bc1qwerty/txid-bot-framework/pkg/store"
)

// CommandHandler handles a Telegram command.
// Return value: response text (empty = no reply).
type CommandHandler func(ctx context.Context, chatID int64, args string) string

// TelegramDispatcher routes Telegram updates to subscribe/unsubscribe
// and any custom command handlers registered by the bot.
type TelegramDispatcher struct {
	tg       *notify.Telegram
	store    *store.Store
	handlers map[string]CommandHandler
	messages DispatcherMessages
}

// DispatcherMessages are the localizable strings.
type DispatcherMessages struct {
	Subscribed   string
	Unsubscribed string
	Unknown      string
	Welcome      string
}

// NewTelegramDispatcher creates a dispatcher wired to the given Telegram + Store.
func NewTelegramDispatcher(tg *notify.Telegram, s *store.Store, msgs DispatcherMessages) *TelegramDispatcher {
	if msgs.Subscribed == "" {
		msgs.Subscribed = "구독 완료. 알림을 받게 됩니다."
	}
	if msgs.Unsubscribed == "" {
		msgs.Unsubscribed = "구독 해지되었습니다."
	}
	if msgs.Unknown == "" {
		msgs.Unknown = "알 수 없는 명령입니다. /start 를 입력하세요."
	}
	if msgs.Welcome == "" {
		msgs.Welcome = "안녕하세요! /subscribe 로 알림을 받을 수 있습니다."
	}
	return &TelegramDispatcher{
		tg:       tg,
		store:    s,
		handlers: make(map[string]CommandHandler),
		messages: msgs,
	}
}

// Handle registers a custom command handler (without the leading slash).
func (d *TelegramDispatcher) Handle(cmd string, h CommandHandler) {
	d.handlers[cmd] = h
}

// Start begins processing updates. Blocks until ctx is cancelled.
func (d *TelegramDispatcher) Start(ctx context.Context) error {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	u.AllowedUpdates = []string{"message", "callback_query"}

	updates := d.tg.API().GetUpdatesChan(u)

	go func() {
		<-ctx.Done()
		d.tg.API().StopReceivingUpdates()
	}()

	for update := range updates {
		if update.Message == nil {
			continue
		}
		if update.Message.IsCommand() {
			d.handleCommand(ctx, update.Message)
		}
	}
	return nil
}

func (d *TelegramDispatcher) handleCommand(ctx context.Context, msg *tgbotapi.Message) {
	chatID := msg.Chat.ID
	cmd := msg.Command()
	args := msg.CommandArguments()

	var reply string
	switch cmd {
	case "start":
		reply = d.messages.Welcome
	case "subscribe":
		if err := d.store.Subscribe(strconv.FormatInt(chatID, 10)); err != nil {
			reply = "구독 실패: " + err.Error()
		} else {
			reply = d.messages.Subscribed
		}
	case "unsubscribe", "stop":
		if err := d.store.Unsubscribe(strconv.FormatInt(chatID, 10)); err != nil {
			reply = "해지 실패: " + err.Error()
		} else {
			reply = d.messages.Unsubscribed
		}
	default:
		if h, ok := d.handlers[cmd]; ok {
			reply = h(ctx, chatID, args)
		} else {
			reply = d.messages.Unknown
		}
	}

	if reply != "" {
		out := tgbotapi.NewMessage(chatID, reply)
		_, _ = d.tg.API().Send(out)
	}
}
