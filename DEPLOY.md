# Deploy procedure for framework-mode bots

This file applies to every bot built on `txid-bot-framework` after the
2026-05-12 consolidation (best-archive, safety-alarm, food-recall,
social-feed, news-summary).

## First deploy from a legacy state file

If the operational state on the target host is one of:

- A JSON file (`processed.json`, `last_post_ids.json`)
- An older SQLite schema without `bot_seen` / `bot_sent`
- A SQLite file using a different filename (e.g. `food_recall.db`
  vs `food-recall.db`)

then the bot's first invocation under framework code creates an empty
`bot_seen` table. Every fetched item is treated as new, which triggers
the full backlog as fresh alerts.

To migrate safely, run the binary **once with `BOOTSTRAP_DEDUP=1`**:

```bash
BOOTSTRAP_DEDUP=1 ./safety-alarm-bot
# or systemd:
# sudo systemctl set-environment BOOTSTRAP_DEDUP=1
# sudo systemctl start safety-alarm-moel.service
# sudo systemctl unset-environment BOOTSTRAP_DEDUP
```

This fetches every source as usual, marks every returned item as seen,
and skips dispatch entirely. The next normal run starts with a hot
`bot_seen` table.

Bots affected on the 2026-05-12 cut:

| Bot | Host | Legacy state | Bootstrap needed? |
|---|---|---|---|
| food-recall | VPS (ubuntu) | `data/food_recall.db` (already has framework `bot_seen`, 88 rows) | No, `resolveDBPath` reuses the legacy filename |
| social-feed | VPS (ubuntu, PM2) | `data/social-feed.db` with old `seen_posts` schema | **Yes** |
| news-summary | dell (swd, systemd) | `data/processed.json` | **Yes** |
| safety-alarm | dell (swd, systemd timer) + GHA | `data/last_post_ids.json` | **Yes** (both dell and GHA) |
| best-archive | wherever it runs | varies | Yes if first framework deploy |

## Routine environment variables

All bots honour these in addition to their own existing config:

| Variable | Default | Effect |
|---|---|---|
| `ARCHIVE_DIR` | `<baseDir>/data/archive` | Raw JSONL backup destination |
| `HEARTBEAT_DIR` | `~/.txid-bots/heartbeats` | Liveness file directory |
| `DB_PATH` (food-recall only) | resolved from baseDir, with legacy fallback | Framework SQLite path |
| `BOOTSTRAP_DEDUP=1` | unset | First-deploy migration mode |
| `NOTIFICATION_HUB_URL`, `NOTIFICATION_SECRET` | unset | When set, the bot pushes per-item events to dash.txid.uk |

## After deploy: smoke checks

1. `tail -n 50` the bot log and confirm a `polling source=...` line.
2. `~/.txid-bots/heartbeats/<bot-name>` exists and `mtime` is recent.
3. `data/archive/<bot-name>/<YYYY-MM-DD>.jsonl` accumulates entries.
4. dash.txid.uk Bot Status widget reports the bot within the next interval.
5. For a Naver Band consumer, the post should be plain text (no raw
   `<b>` / `<a>` tags) — confirms `Message.PlainText` flow.

## Rolling back

```bash
git revert <commit-sha>     # at the bot repo
go build ./...
sudo systemctl restart <service>
```

The framework leaves the legacy `seen_posts` / JSON files untouched, so
a roll-back to the legacy binary resumes dedup against them.
