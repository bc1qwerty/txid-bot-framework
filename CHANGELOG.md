# Changelog

All notable changes to txid-bot-framework. The project is pre-1.0 and
breaking changes are tracked here so downstream bots know what to update.

## v0.3.0

### Added

- **`Config.OnItemMatched`** hook - per-new-item callback that fires
  once per item, but only if at least one subscription's `ItemFilter`
  returned true AND the `Notifier.Send` for that subscription
  succeeded. Runs after the entire per-subscription inner loop has
  finished. Unlike `OnNewItem` (which fires unconditionally before
  dispatch), `OnItemMatched` is the right place for side-effects that
  should stay consistent with what the user actually received, e.g.
  pushing to a notification dashboard. Returning a non-nil error is
  logged but does not abort subsequent items.

### Backward compatibility

- `OnNewItem` still fires unconditionally before the per-sub loop. Bots
  that rely on the old pre-dispatch fire-and-forget semantics do not
  need to change. Bots that need post-delivery consistency should
  switch from `OnNewItem` to `OnItemMatched`.

## Unreleased (local main, 5 commits ahead of origin)

These commits live on the local main branch and have not been pushed.
They will be released as `v0.2.0` once food-recall and bangool finish
side-by-side validation.

### Added

- **`Config.OnNewItem`** hook (96d7cfd) - per-new-item callback that
  fires once after dedup filtering and before the per-subscriber loop.
  Useful for fan-out to external channels (notification hub, structured
  logs). Returning a non-nil error is logged but does not abort dispatch.
- **`TelegramDispatcher.OnSubscribe`** hook (96d7cfd) - async callback
  fired after a successful built-in `/subscribe` command. Used by
  food-recall-bot to deliver a backlog of recent items to brand-new
  subscribers.
- **`TelegramDispatcher.HandleCallback(prefix, h)`** (96d7cfd) - register
  a callback-query handler keyed by the `<prefix>:` part of `query.Data`.
  Lets bots build inline-keyboard UIs.
- **Custom-handler override precedence** (bde39a7) - a `Handle(cmd, h)`
  registration now takes precedence over the built-in `/start`,
  `/subscribe`, `/stop`, `/unsubscribe` defaults. Previously the built-ins
  always won, blocking richer flows like food-recall's
  "/start = subscribe + welcome menu + backlog".
- **`Config.ItemFilter`** hook (a93297a, then refined in 976f5de) -
  per-subscription delivery predicate. Returning false skips that
  subscriber for that item AND does NOT call `MarkSent`, so a later
  filter-state change can re-evaluate within the same `bot_seen`
  lifetime. Enables condition-based bots (bangool, bid-alert).
- **`store.Subscription`** type (976f5de) - `{ID, Recipient, Meta}`
  abstraction that lets one user hold multiple dispatch slots with
  independent sent-tracking. `bot_subscribers` is namespaced and
  per-subscription `MarkSent` tracking lives in `bot_sent`.
- **`store.SubscribeRich(sub)`** + **`store.ActiveSubscriptions()`**
  (976f5de) - new APIs for the Subscription model. Plain `Subscribe(chatID)`
  still works as a shortcut for `SubscribeRich(Subscription{ID: chatID,
  Recipient: chatID})` so existing bots don't need to update.
- **Lazy schema migration** (976f5de) - `store.Open` now ALTERs
  `bot_subscribers` to add `recipient` and `meta` columns if they are
  missing. Older sqlite files upgrade silently on first open. Old rows
  with empty recipient fall back to chat_id-as-recipient on read.
- **`SubscriberFormatter`** optional interface (976f5de) - if a
  `core.Formatter` also implements `FormatFor(sub, item)`, the runner
  calls it instead of `Format(item)` so condition-based bots can embed
  per-subscription context in the rendered message.
- **`TelegramDispatcher.HandleText(h)`** (0178d51) - free-text
  (non-command) message routing. Single handler per dispatcher; bots
  multiplex internally on per-chat conversation state. Enables
  conversation-driven flows like bangool's `/add` walk.

### Changed

- **`ItemFilter` signature** (976f5de) - `func(ctx, chatID string, item)`
  → `func(ctx, sub store.Subscription, item)`. Breaking relative to the
  earlier `a93297a` introduction; no external users existed yet so the
  break is contained to the same release window.

### Backward compatibility

- `Subscribe(chatID)`, `Unsubscribe(chatID)`, `IsSent / MarkSent /
  IsSeen / MarkSeen / ActiveSubscribers / Cleanup` all unchanged.
- The minimal example in `examples/minimal/` still builds and runs
  with zero source changes.
- Bots that don't register an `ItemFilter`, `OnNewItem`, `OnSubscribe`,
  custom callbacks, custom commands, or `HandleText` see identical
  runtime behavior to the previous tag.

### Tests

- 17 unit tests across runner_test.go covering OnNewItem (3),
  ItemFilter (4), Subscription abstraction (5), HandleText
  registration (1), plus the original minimal-example smoke checks.

### Migration notes

If you're upgrading a downstream bot:

1. **No changes needed** if you only use `Subscribe(chatID)`,
   `Unsubscribe`, and `ActiveSubscribers()`. The lazy schema migration
   handles your existing sqlite file automatically.
2. **If you wired `ItemFilter` between commits a93297a and 976f5de**:
   update the signature from `(ctx, chatID, item)` to
   `(ctx, sub store.Subscription, item)`. The `chatID` is now
   `sub.ID` (or `sub.Recipient` if you stored the recipient there).
3. **If you want per-condition dispatch** (multiple sub slots per
   user): use `SubscribeRich` with a composite `ID` like
   `"<chatID>:<condID>"` and stuff the filter fields into `Meta`.
4. **If you need conversation flows**: register `HandleText` and
   maintain per-chat session state in your bot package.

## v0.1.0 (96d7cfd's predecessor, equals origin/main HEAD)

Initial framework with `core.Item / Source / Formatter / Notifier`,
`store.Store` with subscribers + seen + sent tables, `notify.Telegram`
with image fallback + inline keyboards, `bot.Runner` poll loop, basic
`TelegramDispatcher` with hardcoded `/start /subscribe /unsubscribe`.
The `examples/minimal/` RSS bot is the canonical reference.
