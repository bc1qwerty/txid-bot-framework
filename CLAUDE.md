# txid-bot-framework

## Language
- Respond in Korean (한국어로 응답)

## Project Overview
Go framework for building polling bots that fetch items from a source and notify subscribers. Designed for the txid.uk ecosystem but general-purpose. Scaffold for future bot consolidation.

## Tech Stack
- **Language**: Go 1.22
- **Dependencies**: `go-telegram-bot-api/v5`, `modernc.org/sqlite`
- **Pattern**: Interface-based (Source, Formatter, Notifier, Store)

## Package Layout
```
pkg/
  core/
    item.go        # Item, Message, Source, Formatter, Notifier interfaces
  store/
    sqlite.go      # Multi-bot SQLite wrapper with bot_key namespacing
  notify/
    telegram.go    # Telegram notifier with inline keyboards + images
  bot/
    runner.go      # Main Runner (poll → dedup → notify → cleanup)
    handlers.go    # TelegramDispatcher for /subscribe /unsubscribe + custom cmds
examples/
  minimal/
    main.go        # Complete RSS feed bot (~100 lines)
```

## Key Interfaces
```go
type Source interface {
    Name() string
    Fetch(ctx) ([]Item, error)
}
type Formatter interface { Format(item Item) Message }
type Notifier interface { Name() string; Send(ctx, recipient, msg) error }
```

## Usage
```go
runner := bot.New(bot.Config{
    Name: "my-bot",
    Source: source,        // implements core.Source
    Formatter: fmt,        // implements core.Formatter
    Notifier: tg,          // notify.NewTelegram(token)
    Store: st,             // store.Open("./bot.db", "my-bot")
    PollInterval: 10 * time.Minute,
})
runner.Run(ctx)
```

## State Schema
- `bot_subscribers(bot_key, chat_id, active)` - subscriptions
- `bot_seen(bot_key, source, item_id)` - dedup (don't refetch)
- `bot_sent(bot_key, chat_id, item_id)` - per-recipient delivery

## Status
- Framework complete, builds green
- Minimal RSS example works
- **Real bot migration not yet done** - existing bots (food-recall, bid-alert, etc.) still use their own code. Framework waits for new bots or gradual refactor.

## GitHub
https://github.com/bc1qwerty/txid-bot-framework
