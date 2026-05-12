# txid-bot-framework

Advanced Go framework for building scalable polling and realtime bots. Optimized for multi-source scraping and multi-channel notification.

## Features

- **Multi-Source Support**: Merge multiple data fetchers (RSS, API, Scrapers) into a single unified stream via `core.MultiSource`.
- **Multi-Channel Delivery**: Broadcast messages to Telegram, Discord, Naver Band, and Discussions API via `core.MultiNotifier`.
- **AI Data Archiving**: Automatically backups raw items to daily JSONL files for LLM training and RAG ingestion.
- **SQLite Persistence**: Robust deduplication (`IsSeen`, `IsSent`) and subscriber management using SQLite WAL mode.
- **One-Shot & Polling**: Support for both 24/7 loops and cron-style executions (GitHub Actions ready).
- **Stability**: Built-in panic recovery for notifier goroutines and graceful shutdown support.
- **Command Dispatcher**: Full-featured Telegram handler with `/subscribe`, `/unsubscribe`, and custom hooks.

## Package layout

```
pkg/
  core/     Item, Message, MultiSource, MultiNotifier, Source, Formatter, Notifier
  store/    SQLite-backed state (subscribers + dedup)
  notify/   Telegram, Discord, Naver Band, Discussions API implementations
  bot/      Runner (Polling/One-shot), TelegramDispatcher
  archive/  Daily raw data JSONL archiver
```

## Quick start (Multi-Channel Example)

```go
src := core.NewMultiSource(src1, src2)
ntf := core.NewMultiNotifier(telegramNtf, discordNtf)
st, _ := store.Open("./bot.db", "my-bot")

runner := bot.New(bot.Config{
    Name:      "my-bot",
    Source:    src,
    Formatter: &MyFormatter{},
    Notifier:  ntf,
    Store:     st,
})

// One-shot execution (e.g. for Cron)
runner.PollOnce(ctx)

// Or continuous polling
runner.Run(ctx)
```

## State model

The framework uses a shared SQLite schema with `bot_key` namespacing:

- `bot_subscribers`: Per-bot subscription state.
- `bot_seen`: Global deduplication (prevents redundant processing).
- `bot_sent`: Per-recipient delivery tracking (prevents duplicate alerts).
