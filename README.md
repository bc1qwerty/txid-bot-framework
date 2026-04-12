# txid-bot-framework

Go framework for building polling bots that fetch from a data source and notify subscribers.

## Features

- **Source interface**: plug in any data fetcher (RSS, API, web scraping)
- **SQLite state**: subscribers, deduplication (IsSeen/IsSent), retention cleanup
- **Telegram notifier**: built-in, with inline keyboards + image support
- **Command dispatcher**: `/subscribe`, `/unsubscribe`, `/start` out of the box, custom commands via `Handle()`
- **Graceful shutdown**: context-aware runner

## Package layout

```
pkg/
  core/     Item, Message, Source, Formatter, Notifier interfaces
  store/    SQLite-backed state (subscribers + dedup)
  notify/   Telegram notifier implementation
  bot/      Runner + TelegramDispatcher
examples/
  minimal/  Complete RSS feed bot (~100 lines)
```

## Quick start

```go
source := NewMySource()     // implements core.Source
fmt := &MyFormatter{}       // implements core.Formatter
tg, _ := notify.NewTelegram(token)
st, _ := store.Open("./bot.db", "my-bot")

runner := bot.New(bot.Config{
    Name:         "my-bot",
    Source:       source,
    Formatter:    fmt,
    Notifier:     tg,
    Store:        st,
    PollInterval: 10 * time.Minute,
})
runner.Run(ctx)
```

See `examples/minimal/main.go` for a complete RSS feed bot.

## State model

A single SQLite file can host multiple bots. Each bot uses a `botKey`
namespace to isolate its subscribers and dedup state:

- `bot_subscribers(bot_key, chat_id, active)` - subscription state
- `bot_seen(bot_key, source, item_id)` - dedup (don't refetch)
- `bot_sent(bot_key, chat_id, item_id)` - per-recipient delivery

Bot-specific tables can be added via `store.DB()` for custom features.
