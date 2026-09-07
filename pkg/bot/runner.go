// Package bot wires together a Source, Formatter, Notifier, and Store
// into a runnable poller with graceful shutdown.
package bot

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/bc1qwerty/txid-bot-framework/pkg/archive"
	"github.com/bc1qwerty/txid-bot-framework/pkg/core"
	"github.com/bc1qwerty/txid-bot-framework/pkg/store"
)

// Config carries everything needed to run a bot.
// 한 아이템의 발송을 몇 번까지 다시 시도할지. 폴 주기가 봇마다 5~30분이라
// 5회면 대략 25분~2시간 동안 재시도한 뒤 포기한다 — 일시적 429·네트워크 장애를
// 넘기기에 충분하고, 영구 실패가 영원히 재시도되지도 않는다.
const maxSendAttempts = 5

type Config struct {
	// Name identifies this bot for logs and state namespacing.
	Name string

	// ArchiveDir is the base directory for raw JSONL backups. Each bot
	// gets a subdirectory under it. Leave empty to disable archiving.
	ArchiveDir string

	// HeartbeatDir is where PollOnce writes a per-bot liveness timestamp
	// before each fetch. Leave empty to disable heartbeats.
	HeartbeatDir string

	// DisableArchiving forces archiving off even when ArchiveDir is set.
	// Prefer leaving ArchiveDir empty; this flag exists for back-compat.
	DisableArchiving bool

	// ArchiveRetainDays caps how long raw JSONL backups stick around.
	// runCleanup deletes older files daily. 0 disables rotation (files
	// accumulate forever — fine for development, risky for production).
	ArchiveRetainDays int

	// MaxItemsPerPoll caps how many newly-discovered items are dispatched
	// per poll. Items beyond the cap are marked seen — they will NOT be
	// retried later. Use this to prevent flood after extended downtime
	// where a source returns a large backlog. 0 means unlimited.
	MaxItemsPerPoll int

	// BootstrapMode runs one fetch and marks every returned item seen
	// WITHOUT dispatching. Use this on the first deploy of the framework
	// after replacing a legacy state file (processed.json, last_post_ids.
	// json, or a different SQLite schema): otherwise the empty bot_seen
	// table treats every existing source item as new and floods the
	// channel. After a successful bootstrap run, restart the service
	// without the flag.
	BootstrapMode bool

	// Source fetches new items.
	Source core.Source

	// Formatter converts Items to Messages.
	Formatter core.Formatter

	// Notifier delivers messages to recipients.
	Notifier core.Notifier

	// Store persists subscribers + dedup state.
	Store *store.Store

	// PollInterval is how often Fetch is called.
	PollInterval time.Duration

	// InitialDelay before the first poll (optional).
	InitialDelay time.Duration

	// RetainDuration is how long to keep seen/sent records.
	// If zero, defaults to 90 days.
	RetainDuration time.Duration

	// OnError is called when polling hits an error (optional).
	// Useful for sending admin notifications.
	OnError func(err error)

	// ErrorThrottle coalesces consecutive identical errors so a long
	// outage does not spam the OnError sink. When non-zero, an error
	// whose .Error() string matches the previous fire within this window
	// is silently suppressed (the runner still logs it locally).
	// Zero (default) fires every error - existing behavior.
	ErrorThrottle time.Duration

	// OnPollComplete is called at the end of every PollOnce, regardless
	// of whether any new items were dispatched. The intended use is to
	// nudge a liveness sink (e.g., notifyhub.LogPush) so dashboards do
	// not flag low-volume bots as stale during long quiet stretches.
	//
	// Receives the total number of fresh items that reached the
	// dispatch loop (after dedup, before subscription filter). A non-
	// nil return is logged but not fatal.
	OnPollComplete func(ctx context.Context, newItemCount int) error

	// OnNewItem is called for each newly-fetched item before dispatch.
	// Runs after dedup filtering, once per item regardless of subscriber count.
	// Useful for fan-out to external channels (notification hub, logs).
	// A non-nil return is logged, not fatal - dispatch still proceeds.
	OnNewItem func(ctx context.Context, item core.Item) error

	// OnItemMatched is called exactly once per newly-fetched item, but only
	// if at least one subscription's ItemFilter returned true for that item.
	// It fires after all per-subscription Send attempts have completed.
	//
	// Use this (instead of OnNewItem) for side-effects that should be
	// skipped when an item would not reach any user, for example pushing
	// to a notification dashboard that should stay consistent with what
	// was actually delivered on Telegram.
	//
	// A non-nil return is logged, not fatal.
	OnItemMatched func(ctx context.Context, item core.Item) error

	// ItemFilter decides whether a given Subscription should receive a
	// given item. Return true to deliver, false to skip.
	//
	// When nil, all items are delivered to all active Subscriptions
	// (default broadcast behavior). When non-nil, filtered-out
	// (item, sub) pairs are NOT marked as sent, so a later filter change
	// can deliver them within the same bot_seen lifetime.
	//
	// The Subscription.ID is what MarkSent uses, and Subscription.Meta
	// carries whatever per-sub state the bot stored via SubscribeRich.
	//
	// Performance note: this runs O(items × subscriptions) per poll. For
	// bots with heavy per-sub filter logic, load everything into a
	// closure at poll start rather than querying inside the filter body.
	ItemFilter func(ctx context.Context, sub store.Subscription, item core.Item) bool
}

// SubscriberFormatter is an optional interface a core.Formatter can
// implement to customize the rendered Message per Subscription. When
// the Runner detects this interface, it calls FormatFor instead of the
// basic Format(item). This is how condition-based bots (bangool,
// bid-alert) can include "which condition triggered" context in the
// alert text.
type SubscriberFormatter interface {
	FormatFor(sub store.Subscription, item core.Item) core.Message
}

// Runner executes the poll loop.
type Runner struct {
	cfg      Config
	log      *log.Logger
	archiver *archive.Archiver // nil when ArchiveDir is empty or DisableArchiving

	// Error throttling state. Not protected by a mutex because pollOnce
	// is called from a single goroutine in production (Run's ticker loop)
	// and tests drive it serially.
	lastErrMsg  string
	lastErrTime time.Time
}

// New creates a Runner from config.
func New(cfg Config) *Runner {
	r := &Runner{
		cfg: cfg,
		log: log.New(log.Writer(), "["+cfg.Name+"] ", log.LstdFlags),
	}
	if cfg.ArchiveDir != "" && !cfg.DisableArchiving {
		r.archiver = archive.NewLocalArchiver(cfg.ArchiveDir)
	}
	return r
}

// Run starts the poll loop and blocks until ctx is cancelled.
func (r *Runner) Run(ctx context.Context) error {
	r.log.Printf("runner started: interval=%s", r.cfg.PollInterval)

	if r.cfg.InitialDelay > 0 {
		select {
		case <-time.After(r.cfg.InitialDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Heartbeat ticker — keeps ~/.txid-bots/heartbeats/<bot> fresh
	// even between polls. dash.txid.uk does not read this file directly,
	// but ssh-based liveness checks (and any future agent) rely on it.
	// Skip for one-shot runs (PollInterval==0) and when HeartbeatDir is
	// empty.
	if r.cfg.HeartbeatDir != "" && r.cfg.PollInterval > 0 {
		go r.heartbeatTicker(ctx)
	}

	// Initial poll
	r.PollOnce(ctx)

	// Bootstrap mode is one-shot even inside Run(): the first poll has
	// marked every backlog item seen, and we want the operator to
	// restart the service without BOOTSTRAP_DEDUP for normal dispatch.
	if r.cfg.BootstrapMode {
		r.log.Println("bootstrap complete — exit. Restart without BOOTSTRAP_DEDUP for normal dispatch.")
		return nil
	}

	// Start cleanup goroutine
	go r.runCleanup(ctx)

	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			r.log.Println("runner stopped")
			return ctx.Err()
		case <-ticker.C:
			r.PollOnce(ctx)
		}
	}
}

// PollOnce runs a single fetch-filter-notify cycle.
func (r *Runner) PollOnce(ctx context.Context) {
	archive.RecordHeartbeat(r.cfg.HeartbeatDir, r.cfg.Name)
	source := r.cfg.Source.Name()
	// 아이템별 dedup 네임스페이스. MultiSource 는 하위 소스 이름을 스탬프하므로
	// 크롤러 하나를 껐다 켜도 그 크롤러의 행만 영향받는다. 스탬프가 없으면
	// (단일 소스 봇) 예전과 같이 소스 이름을 쓴다.
	itemSource := func(it core.Item) string {
		if it.Source != "" {
			return it.Source
		}
		return source
	}
	r.log.Printf("polling source=%s", source)

	items, err := r.cfg.Source.Fetch(ctx)
	if err != nil {
		// Partial success is possible for composite sources (MultiSource):
		// we still dispatch whatever was returned so a single failing
		// sub-source does not block all the others. err is surfaced
		// through OnError for observability.
		r.log.Printf("fetch error: %v", err)
		r.invokeOnError(err)
		if len(items) == 0 {
			return
		}
		r.log.Printf("fetch partial success: dispatching %d items despite errors", len(items))
	}

	if r.archiver != nil {
		if err := r.archiver.Archive(r.cfg.Name, items); err != nil {
			r.log.Printf("archiving failed: %v", err)
		}
		// Best-effort rotation for one-shot bots that never enter
		// runCleanup. Cheap (stat-only) when no files are stale.
		if r.cfg.ArchiveRetainDays > 0 {
			if removed, err := r.archiver.Rotate(r.cfg.ArchiveRetainDays); err == nil && removed > 0 {
				r.log.Printf("archive rotate: removed %d old jsonl files", removed)
			}
		}
	}

	if len(items) == 0 {
		r.log.Printf("no items fetched")
		return
	}

	// Filter out items we've already seen
	newItems := make([]core.Item, 0, len(items))
	for _, item := range items {
		seen, err := r.cfg.Store.IsSeen(itemSource(item), item.ID)
		if err != nil {
			r.log.Printf("seen check error: %v", err)
			continue
		}
		if !seen {
			newItems = append(newItems, item)
		}
	}

	if len(newItems) == 0 {
		r.log.Printf("no new items")
		if r.cfg.OnPollComplete != nil {
			if err := r.cfg.OnPollComplete(ctx, 0); err != nil {
				r.log.Printf("OnPollComplete hook error: %v", err)
			}
		}
		return
	}

	// Bootstrap mode — mark every new item seen and skip dispatch entirely.
	// This is the safe migration path from a legacy state file to the
	// framework's bot_seen table on a freshly initialized DB.
	if r.cfg.BootstrapMode {
		r.log.Printf("BOOTSTRAP: marking %d items seen without dispatch", len(newItems))
		for _, it := range newItems {
			if err := r.cfg.Store.MarkSeen(itemSource(it), it.ID); err != nil {
				r.log.Printf("bootstrap mark seen error: %v", err)
			}
		}
		return
	}

	// Backlog cap — when configured, drop oldest excess and mark them seen
	// so they do not re-enter the queue next poll. This is the framework
	// equivalent of legacy "backlogCap" controls in pre-merger bots.
	if r.cfg.MaxItemsPerPoll > 0 && len(newItems) > r.cfg.MaxItemsPerPoll {
		excess := newItems[r.cfg.MaxItemsPerPoll:]
		newItems = newItems[:r.cfg.MaxItemsPerPoll]
		r.log.Printf("backlog cap: dispatching %d, marking %d excess as seen",
			len(newItems), len(excess))
		for _, it := range excess {
			if err := r.cfg.Store.MarkSeen(itemSource(it), it.ID); err != nil {
				r.log.Printf("mark seen (cap excess) error: %v", err)
			}
		}
	}

	// Get subscriptions (the framework iterates these, not raw chat_ids,
	// so bots with per-user filter conditions can register one
	// Subscription per condition and keep dedup independent per slot).
	subs, err := r.cfg.Store.ActiveSubscriptions()
	if err != nil {
		r.log.Printf("subscriptions error: %v", err)
		return
	}

	subFormatter, _ := r.cfg.Formatter.(SubscriberFormatter)

	r.log.Printf("dispatching %d new items to %d subscriptions", len(newItems), len(subs))

	// Notify
	for _, item := range newItems {
		if r.cfg.OnNewItem != nil {
			if err := r.cfg.OnNewItem(ctx, item); err != nil {
				r.log.Printf("OnNewItem hook error (item=%s): %v", item.ID, err)
			}
		}

		// Base message is computed once when there is no sub-aware formatter.
		var baseMsg core.Message
		if subFormatter == nil {
			baseMsg = r.cfg.Formatter.Format(item)
		}

		var matched bool
		// ⚠ 발송 실패를 기억한다(2026-09-08 추가). 예전에는 Send 가 실패해도 아래
		//   MarkSeen 이 무조건 실행돼, 다음 폴의 IsSeen 게이트가 그 아이템을 걸러
		//   **재시도 기회가 영영 없었다**. 텔레그램이 429 를 잠깐 내기만 해도 그
		//   알림은 영구 유실되고 흔적은 로그 한 줄뿐이었다(격리 재현: 소스가 같은
		//   아이템을 계속 줘도 재발송 시도 0회). 이 프레임워크를 쓰는 모든 봇에
		//   공통이었다.
		var sendFailed bool
		var lastSendErr error
		for _, sub := range subs {
			sent, err := r.cfg.Store.IsSent(sub.ID, item.ID)
			if err != nil || sent {
				continue
			}
			if r.cfg.ItemFilter != nil && !r.cfg.ItemFilter(ctx, sub, item) {
				// Intentionally NOT marking sent: if the filter state
				// changes before the item ages out of bot_seen, the next
				// poll can re-evaluate and potentially deliver it.
				continue
			}
			msg := baseMsg
			if subFormatter != nil {
				msg = subFormatter.FormatFor(sub, item)
			}
			if err := r.cfg.Notifier.Send(ctx, sub.Recipient, msg); err != nil {
				r.log.Printf("send error sub=%s recipient=%s: %v", sub.ID, sub.Recipient, err)
				sendFailed = true
				lastSendErr = err
				continue
			}
			matched = true
			if err := r.cfg.Store.MarkSent(sub.ID, item.ID); err != nil {
				r.log.Printf("mark sent error: %v", err)
			}
		}

		if matched && r.cfg.OnItemMatched != nil {
			if err := r.cfg.OnItemMatched(ctx, item); err != nil {
				r.log.Printf("OnItemMatched hook error (item=%s): %v", item.ID, err)
			}
		}

		// 실패한 구독자가 있으면 seen 처리를 미뤄 다음 폴에서 다시 시도한다.
		// 재시도는 이미 안전하다 — 위 IsSent 게이트가 성공한 구독자에게 중복
		// 발송되는 것을 막으므로 실패한 (아이템, 구독자) 쌍만 다시 간다.
		// ⚠ 다만 무한 재시도는 안 된다. chat not found·bot blocked 같은 영구 실패는
		//   영원히 성공하지 않으므로, 시도 횟수를 세어 상한을 넘기면 포기하고
		//   seen 처리하되 그때는 로그가 아니라 OnError 로 승격시킨다 —
		//   조용히 버리는 것이 애초에 이 결함의 본질이었다.
		if sendFailed {
			attempts, err := r.cfg.Store.RecordSendFailure(itemSource(item), item.ID, fmt.Sprint(lastSendErr))
			if err != nil {
				r.log.Printf("record send failure error: %v", err)
			}
			if attempts < maxSendAttempts {
				r.log.Printf("send retry pending item=%s attempts=%d/%d", item.ID, attempts, maxSendAttempts)
				continue // seen 처리하지 않는다 → 다음 폴에서 재시도
			}
			r.log.Printf("send gave up item=%s after %d attempts: %v", item.ID, attempts, lastSendErr)
			r.invokeOnError(fmt.Errorf("발송 %d회 실패로 포기 item=%s: %w", attempts, item.ID, lastSendErr))
		}
		if err := r.cfg.Store.ClearSendFailure(itemSource(item), item.ID); err != nil {
			r.log.Printf("clear send failure error: %v", err)
		}

		if err := r.cfg.Store.MarkSeen(itemSource(item), item.ID); err != nil {
			r.log.Printf("mark seen error: %v", err)
		}
	}

	if r.cfg.OnPollComplete != nil {
		if err := r.cfg.OnPollComplete(ctx, len(newItems)); err != nil {
			r.log.Printf("OnPollComplete hook error: %v", err)
		}
	}
}

// invokeOnError fires the user-supplied OnError hook with optional
// throttling. When ErrorThrottle is set, identical consecutive errors
// inside the window are suppressed (still logged locally above).
func (r *Runner) invokeOnError(err error) {
	if r.cfg.OnError == nil || err == nil {
		return
	}
	if r.cfg.ErrorThrottle > 0 {
		msg := err.Error()
		if msg == r.lastErrMsg && time.Since(r.lastErrTime) < r.cfg.ErrorThrottle {
			return
		}
		r.lastErrMsg = msg
		r.lastErrTime = time.Now()
	}
	r.cfg.OnError(err)
}

// heartbeatTicker pings the liveness file every 15 minutes so a
// daemon with a long PollInterval (food-recall: 4h) does not look
// stale between polls. Cheap (one writeFile).
func (r *Runner) heartbeatTicker(ctx context.Context) {
	tick := 15 * time.Minute
	if r.cfg.PollInterval < tick {
		// No point ticking faster than polling already does.
		return
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			archive.RecordHeartbeat(r.cfg.HeartbeatDir, r.cfg.Name)
		}
	}
}

// runCleanup periodically purges old records.
func (r *Runner) runCleanup(ctx context.Context) {
	retain := r.cfg.RetainDuration
	if retain == 0 {
		retain = 90 * 24 * time.Hour
	}

	// Run daily
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.cfg.Store.Cleanup(retain); err != nil {
				r.log.Printf("cleanup error: %v", err)
			} else {
				r.log.Printf("cleanup done (retain=%s)", retain)
			}
			if r.archiver != nil && r.cfg.ArchiveRetainDays > 0 {
				if removed, err := r.archiver.Rotate(r.cfg.ArchiveRetainDays); err != nil {
					r.log.Printf("archive rotate error: %v", err)
				} else if removed > 0 {
					r.log.Printf("archive rotate: removed %d old jsonl files", removed)
				}
			}
		}
	}
}
