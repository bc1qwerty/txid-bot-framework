# Bot Migration Guide

How to port an existing Go Telegram bot to txid-bot-framework. Patterns
and limits learned from migrating food-recall-bot and bangool.

## When the framework is a good fit

Your bot maps cleanly if it has:

- A polling loop that fetches items from one source and broadcasts to
  multiple subscribers
- Per-subscriber `IsSent` dedup so each user gets each item once
- Telegram as the delivery channel (or willingness to add a
  `core.Notifier` implementation for another channel)

Two reference shapes:

1. **Simple broadcast** (food-recall-bot). One source, all active
   subscribers receive every new item. Fits the framework's
   `Subscribe(chatID)` API directly.
2. **Per-condition broadcast** (bangool, bid-alert). One source, each
   subscriber holds N filter conditions and receives items that match
   any of them. Fits the framework's `Subscription` abstraction with
   composite IDs like `"{chatID}:{conditionID}"`.

## When the framework is NOT a good fit (yet)

The framework's `bot.Runner` assumes:

- **One Source** per bot. If your bot polls multiple unrelated APIs
  in parallel and merges results (best-archive-bot's 13 community
  scrapers), you need to either build a meta-Source that fans out
  internally or wait for first-class multi-Source support.
- **One Notifier** per bot. If your bot delivers to multiple channels
  with different formatting (safety_alarm_bot writes to Telegram AND
  Naver Band), you need to build a fan-out Notifier wrapper or wait
  for first-class multi-Notifier support.
- **Long-running polling loop**. If your bot is a `run_once` cron job
  (best-archive, safety_alarm via GitHub Actions), the framework's
  `bot.Runner.Run(ctx)` blocking semantics don't match. You can call
  `runner.pollOnce(ctx)` once and exit, but the cleanup goroutine
  still spawns - workable but not idiomatic.
- **Fixed channel broadcast**. If your bot sends to a single hardcoded
  `CHAT_ID` rather than dynamic subscribers, the framework store is
  overkill. You can still use `Source / Formatter / Notifier` directly
  without the runner, but the win is small.

## Core abstractions

### `core.Source`

```go
type Source interface {
    Name() string
    Fetch(ctx context.Context) ([]Item, error)
}
```

`Name()` returns a stable string used as the `source` column in
`bot_seen`. Avoid changing it - if you do, every previously-seen item
will look new and re-broadcast.

`Fetch` returns ALL currently-known items. The framework dedups via
`bot_seen` so returning the same items repeatedly is safe and cheap.
For sources with a "since N" API (RSS, REST), pass an internal
high-watermark; for full-snapshot APIs, just return everything.

### `core.Formatter` and `bot.SubscriberFormatter`

```go
type Formatter interface {
    Format(item Item) Message
}

// Optional - implement BOTH on the same type to enable per-sub messages
type SubscriberFormatter interface {
    FormatFor(sub store.Subscription, item Item) Message
}
```

If only `Format` is implemented, the runner computes the message once
per item and reuses it for every subscriber - cheap and correct for
broadcast bots.

If `SubscriberFormatter` is also implemented, the runner calls
`FormatFor(sub, item)` per subscriber. Use this when you need
condition context in the message (bangool's "조건 #42 매칭" line) or
per-language localization (read `sub.Meta["lang"]`).

### `core.Notifier`

```go
type Notifier interface {
    Name() string
    Send(ctx context.Context, recipient string, msg Message) error
}
```

The `recipient` is whatever the runner pulls from
`Subscription.Recipient`. For Telegram that's a chat_id as a string.
If you need a different channel, implement this interface and pass
your custom Notifier to `bot.Config`.

### `store.Subscription`

```go
type Subscription struct {
    ID        string            // unique within bot_key, used as the dedup key
    Recipient string            // passed to Notifier.Send as `recipient`
    Meta      map[string]string // arbitrary filter/format state (JSON)
}
```

The framework persists every Subscription in `bot_subscribers` and
tracks dispatch in `bot_sent` keyed by `(bot_key, sub.ID, item.ID)`.

For simple broadcast bots, `Subscribe(chatID)` is a shortcut for
`SubscribeRich(Subscription{ID: chatID, Recipient: chatID})`. There's
no reason to use `SubscribeRich` directly unless you need composite IDs
or Meta.

For per-condition bots, register one Subscription per condition with
ID `"{chatID}:{conditionID}"` and stuff the filter parameters into
`Meta`. The runner will then loop subscriptions (not chats) and call
`ItemFilter(sub, item)` once per (item, sub) pair.

## Hooks (all optional)

### `Config.OnNewItem(ctx, item) error`

Fires once per item after dedup, before the per-subscriber loop. Use
for fan-out to external channels: notification hub push, structured
log emission, metrics counter. A non-nil return is logged and not
fatal - the per-subscriber loop still runs.

### `Config.ItemFilter(ctx, sub, item) bool`

Fires per (item, subscription) pair. Return true to deliver, false to
skip. A skipped (item, sub) pair does NOT update `bot_sent`, so a
later filter-state change can deliver it as long as the item is still
in `bot_seen`. NOTE: items already in `bot_seen` from a previous poll
are NEVER re-evaluated; if you need backfill on filter change, do it
explicitly (see food-recall's `sendBacklog` pattern).

### `Config.OnError(err)`

Fires when `Source.Fetch` returns an error. Currently NOT
rate-limited - a persistent API outage will spam your admin chat
unless you wrap the callback yourself. (Future: framework-level
coalescing.)

### `TelegramDispatcher.OnSubscribe(hook)`

Async callback fired after a successful built-in `/subscribe`. Used
for backlog delivery to new subscribers. Note: if you override
`/subscribe` with `dispatcher.Handle("subscribe", ...)`, this hook
does NOT fire from your custom path - call your post-subscribe logic
directly inside the handler.

### `TelegramDispatcher.HandleText(handler)`

Routes any non-command, non-callback message to your handler. Single
registration per dispatcher; multiplex on per-chat session state
internally. Required for conversation-driven flows like bangool's
`/add` walk that asks the user for area, price, and apt-name as text.

### `TelegramDispatcher.HandleCallback(prefix, handler)`

Routes callback queries with `Data` starting with `"<prefix>:"`. Build
inline keyboard UIs by combining multiple prefixes. Examples from
bangool: `sido`, `sigungu`, `region`, `ptype`, `ttype`, `area`,
`price`, `aptname`, `confirm`.

## Recommended migration steps

1. **Create a feature branch** in the bot repo (`framework-migration`).
2. **Add the framework as a dependency** with a local `replace`
   directive while you iterate: `go mod edit
   -replace=github.com/bc1qwerty/txid-bot-framework=/path/to/framework`.
   Remove the replace before merging.
3. **Create `cmd/framework/` directory** alongside the legacy `main.go`
   so the two binaries can coexist. Keep the legacy code untouched.
4. **Port the pieces in this order**:
   1. `mapper.go` - if you have per-condition state, write the
      `dbCondition ↔ Subscription` round-trip first. Pure functions,
      easy to unit test.
   2. `filter.go` - reimplement your matching logic over
      `(sub Subscription, item core.Item)`. Pure functions.
   3. `formatter.go` - implement `core.Formatter` and (optionally)
      `SubscriberFormatter` using your existing format helpers.
   4. `source.go` - wrap your existing API client + db cache with
      injected closures so the orchestration is unit-testable.
   5. `migration/migrate.sql` + `verify.sql` + `rollback.sql` - copy
      legacy tables into `bot_subscribers / bot_seen / bot_sent`.
      Validate against a mock sqlite before touching production.
   6. `main.go` - wire everything into `bot.Runner`. Add a safety
      guard env var so a stray run can't grab the production token.
   7. **Command UX last**: port `/start /list /delete` as
      `dispatcher.Handle(cmd, ...)` calls. For conversation-driven
      `/add` flows, port the state machine into `add_flow.go` and
      register `HandleText` + the relevant callback prefixes.
5. **Side-by-side validation**: with a separate test bot token and a
   separate DB file, run the new binary for 24-48 hours alongside the
   legacy one. Compare alerts.
6. **CUTOVER**: write a runbook (see food-recall and bangool's
   `migration/CUTOVER.md` for templates) and execute it.

## Anti-patterns

- **Don't dual-write subscribers** to both legacy `subscribers` and
  framework `bot_subscribers` tables. Pick one source of truth. The
  framework store is the simpler choice once you've migrated; the
  legacy table can be dropped after a stable cutover.
- **Don't change `Source.Name()` after launch.** Every previously-seen
  item becomes new again and re-broadcasts. If you must rename, run a
  one-off `UPDATE bot_seen SET source='new' WHERE source='old'`.
- **Don't put slow code in `ItemFilter`.** It runs O(items × subs) per
  poll. Pre-load any per-sub state into a closure at poll start.
- **Don't query the framework store from inside `Source.Fetch`** in a
  way that races with the runner's own writes. Use the legacy domain
  cache for stateful queries; the framework store is for membership +
  dispatch state only.

## Reference implementations

- **food-recall-bot** (`/data/projects/food-recall-bot/cmd/framework/`):
  simple broadcast. Source/Formatter/Notifier wired directly,
  `Subscribe(chatID)`, custom `/start` with menu + backlog, `recent:*`
  pagination via `HandleCallback`.
- **bangool / realestate-alert-bot**
  (`/data/projects/realestate-alert-bot/cmd/framework/`): per-condition
  broadcast. Composite Subscription IDs, `ItemFilter` consuming Meta,
  `SubscriberFormatter` for condition-aware messages, full
  conversation-driven `/add` walk via `HandleText` + 9 callback
  prefixes.
- **examples/conditional/** (this repo): minimal hand-coded reference
  for the per-condition pattern - read this first if you're porting a
  new condition-based bot.
