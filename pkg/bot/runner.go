// Package bot wires together a Source, Formatter, Notifier, and Store
// into a runnable poller with graceful shutdown.
package bot

import (
	"context"
	"errors"
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

	// OnRecipientDeactivated is called after a permanent send failure
	// (blocked bot, deleted account, chat not found) made the runner
	// deactivate every subscription of that recipient. Use it to keep
	// bot-side mirrors of subscription state (e.g. bangool's legacy
	// conditions table) in sync — otherwise those rows stay active,
	// keep feeding poll targets, and can resurrect the dead
	// subscription on the next migration run. At most once per
	// recipient per poll.
	OnRecipientDeactivated func(recipient string)

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

	// ⚠**종료 신호로 끊긴 폴은 장애가 아니다.** 아래 분기는 그것을 모르고
	//   `fetch error: … context canceled` 를 찍은 뒤 OnError 까지 불렀고, 그 줄이
	//   그대로 사람 경보로 올라갔다(social-feed 2026-09-12, VPS 재시작마다 🔴).
	//   더 나쁜 것은 그 다음이다 — 이미 죽은 ctx 로 **디스패치를 계속했다**.
	//   OnNewItem 이 취소된 ctx 로 허브를 찔러 실패하고(nara-bot 2026-09-10
	//   「허브 push 실패 23건」이 이것이다), 발송도 전부 실패해 그 아이템들의
	//   SendFailureAttempts 가 올라간다. 그러면 다음 기동에서 `attempts != 0` 이라
	//   **OnNewItem 훅이 아예 건너뛰어져 허브 푸시가 영영 유실된다.**
	//   재시작 중에는 아무것도 하지 않고 나간다. 남은 아이템은 seen 처리가 안 됐으니
	//   다음 기동의 첫 폴이 정상적으로 다시 집는다.
	//   ⚠**게이트는 「취소」만 본다 — deadline 만료는 그대로 경보한다.** 처음엔
	//   `ctx.Err() != nil` 로 뒀는데, 그러면 폴 전체를 타임아웃으로 감싸는 원샷 봇
	//   (safety_alarm_bot·best-archive-bot 은 `WithTimeout(5분)` 뒤 PollOnce)의
	//   **「크롤이 예산을 넘겼다」는 진짜 신호까지 통째로 삼킨다.** 그쪽은 사람이
	//   봐야 하는 고장이다. 종료 신호는 Canceled, 예산 초과는 DeadlineExceeded 로
	//   갈라서 온다.
	if errors.Is(ctx.Err(), context.Canceled) {
		r.log.Printf("poll canceled by shutdown")
		return
	}

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
	//
	// ⚠ 이 폐기는 **영구적**이다. 여기서 seen 이 된 아이템은 다시는 후보가 되지
	//   않으므로, 사용자가 받았어야 할 알림이 통째로 사라진다. 그런데 오래도록
	//   흔적이 로그 한 줄뿐이었고 그 줄에는 error 계열 낱말이 없어 함대의 원격
	//   로그 감시(check-vps-errors 의 `Error|error|FATAL|...`)에 **걸리지 않았다.**
	//   best-archive 는 2026-09-12~14 에 16회 중 5회가 상한에 걸려 17건을 잃었는데,
	//   감사 때 사람이 로그를 눈으로 읽어서야 알았다.
	// 🔑 같은 파일 아래쪽(발송 포기)은 이미 «seen 처리하되 그때는 로그가 아니라
	//   OnError 로 승격시킨다» 는 원칙을 세워 뒀다. 영구 폐기라는 점이 똑같은데
	//   이 자리만 그 원칙에서 빠져 있었다 — 정책이 일부 경로에만 걸려 있던 것이다.
	if r.cfg.MaxItemsPerPoll > 0 && len(newItems) > r.cfg.MaxItemsPerPoll {
		excess := newItems[r.cfg.MaxItemsPerPoll:]
		newItems = newItems[:r.cfg.MaxItemsPerPoll]
		// ⚠ 문구에 "error" 를 넣는 것은 미관이 아니라 **계약**이다 — 원격 로그
		//   감시가 `Error|error|FATAL|...` 로 훑는다. 그리고 OnError 를 배선하지
		//   않은 봇(social-feed 가 그렇다)에서는 이 줄이 유일한 흔적이다.
		r.log.Printf("backlog cap error: dispatched %d, dropped %d items permanently (marked seen, never retried)",
			len(newItems), len(excess))
		for _, it := range excess {
			if err := r.cfg.Store.MarkSeen(itemSource(it), it.ID); err != nil {
				r.log.Printf("mark seen (cap excess) error: %v", err)
			}
		}
		r.invokeOnError(fmt.Errorf(
			"backlog cap error: %d items dropped permanently — 백로그 상한으로 %d건을 발송 없이 영구 폐기했다"+
				" (MaxItemsPerPoll=%d, 이번 폴 신규 %d건). 상한을 올리거나 폴 간격을 줄일 것",
			len(excess), len(excess), r.cfg.MaxItemsPerPoll, len(excess)+len(newItems)))
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

	// 이번 폴에서 영구 실패로 끊은 수신자. subs 는 폴 시작 때 한 번 읽은 스냅샷이라,
	// 여기 기록해 두지 않으면 **같은 사람의 남은 슬롯과 뒤이은 아이템에 계속 다시
	// 보낸다**(nara-bot 처럼 한 사람이 조건별 슬롯을 여럿 가지는 봇에서 두드러진다).
	// DB 는 이미 꺼졌지만 스냅샷은 그것을 모른다.
	deadRecipients := make(map[string]bool)

	// Notify
	for _, item := range newItems {
		// ⚠ 위 종료 게이트는 Fetch 직후 한 번뿐이라, 허브 페이싱으로 디스패치가
		//   몇 분씩 걸리는 봇(nara-bot 44건≈7분)은 그 사이 SIGTERM 이 올 수 있다.
		//   그때 OnNewItem 은 죽은 ctx 로 허브를 찔러 실패(로그 한 줄)하는데
		//   텔레그램 발송은 ctx 를 안 받아 성공한다 → seen 처리돼 그 아이템의
		//   허브 푸시가 영영 유실된다. 여기서 멈추면 남은 아이템은 seen 이 안
		//   됐으니 다음 기동의 첫 폴이 정상적으로 다시 집는다.
		if errors.Is(ctx.Err(), context.Canceled) {
			r.log.Printf("dispatch aborted by shutdown")
			return
		}
		// ⚠**훅의 «아이템당 한 번» 은 성공 기록(bot_hook_done)으로 지킨다.**
		//   예전에는 «발송 실패 카운터가 0 인가» 로 흉내 냈다(nara-bot 8중 푸시 사고의
		//   수리). 그 근사가 두 방향으로 샜다:
		//   ① 훅 실패 + 발송 실패 → 재시도 폴에서 attempts!=0 이라 훅을 영영 건너뜀.
		//   ② 훅 실패 + 발송 성공 → 그대로 seen 처리. 위 종료 게이트 주석의 실측이
		//      정확히 이 모양이다 — SIGTERM 중 죽은 ctx 로 허브를 찔러 실패하고
		//      텔레그램은 ctx 를 안 받아 성공, 허브 푸시만 영영 유실.
		//   이제 성공하면 기록하고, 실패하면 발송 실패와 같은 경로(seen 유예 +
		//   bot_send_failure 카운터 공유)로 다음 폴에 재시도한다. 상한도 같이 쓴다 —
		//   허브가 오래 죽어 있으면 5회 뒤 포기가 OnError 로 승격된다.
		//   ⚠조회·기록 실패는 «안 했다» 로 간주한다 — 한 번 더 부르는 쪽이 안전하다
		//   (허브 중복은 성가시지만 유실은 되돌릴 수 없다).
		var hookErr error
		if r.cfg.OnNewItem != nil {
			done, err := r.cfg.Store.IsHookDone(itemSource(item), item.ID)
			if err != nil {
				r.log.Printf("hook-done lookup error (item=%s): %v", item.ID, err)
			}
			if !done {
				if hookErr = r.cfg.OnNewItem(ctx, item); hookErr != nil {
					r.log.Printf("OnNewItem hook error (item=%s): %v", item.ID, hookErr)
				} else if err := r.cfg.Store.MarkHookDone(itemSource(item), item.ID); err != nil {
					r.log.Printf("mark hook done error (item=%s): %v", item.ID, err)
				}
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
		// ⚠OnItemMatched 의 재시도 중복 게이트(OnNewItem 의 v0.10.0 게이트와 형제).
		//   부분 발송 실패로 seen 이 미뤄진 아이템은 다음 폴에 다시 오는데, 남은
		//   구독자에게 성공하면 matched 가 또 true 가 돼 허브에 같은 아이템이
		//   다시 푸시됐다. IsSent 행이 하나라도 있으면 이전 폴에서 이미 발화한
		//   것이다. SendFailureAttempts 게이트를 그대로 쓰면 «전원 실패 후
		//   성공» 폴의 첫 발화까지 삼키므로 여기엔 부적합하다.
		var alreadyDelivered bool
		for _, sub := range subs {
			if deadRecipients[sub.Recipient] {
				continue
			}
			sent, err := r.cfg.Store.IsSent(sub.ID, item.ID)
			if err != nil {
				// ⚠"조회 실패"와 "이미 보냈다"를 같이 continue 하던 자리다. 그러면
				//   아이템이 루프 끝에서 seen 처리되고 **이 구독자는 그 알림을 영영
				//   못 받는다.** DB 가 잠깐 흔들린 것과 보낼 필요가 없는 것은 전혀
				//   다른데 결과가 같았고, 로그조차 남지 않아 흔적도 없었다.
				//   판단할 수 없으면 미룬다 — 아래 sendFailed 경로가 다음 폴에서
				//   다시 시도하게 한다.
				r.log.Printf("is-sent lookup error sub=%s item=%s: %v", sub.ID, item.ID, err)
				sendFailed = true
				lastSendErr = err
				continue
			}
			if sent {
				alreadyDelivered = true
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
				// ⚠영구 수신 실패(차단·계정 삭제·chat not found)는 **재시도 대상이
				//   아니다.** 아래 maxSendAttempts 상한은 아이템 단위라, 새 아이템이
				//   올 때마다 카운터가 0 부터 다시 시작해 상한이 아무것도 막지 못한다.
				//   nara-bot 에서 차단된 구독 하나가 새 공고마다 `Forbidden: bot was
				//   blocked by the user` 를 찍어 로그를 채우고 있었다(2026-09-09).
				//   구독을 끄고 사람에게 알린 다음 넘어간다 — 다른 구독자에게는
				//   이 아이템이 정상 발송돼야 하므로 sendFailed 로 세지 않는다.
				if core.IsPermanentRecipient(err) {
					deadRecipients[sub.Recipient] = true
					n, derr := r.cfg.Store.DeactivateRecipient(sub.Recipient)
					if derr != nil {
						r.log.Printf("deactivate recipient error recipient=%s: %v", sub.Recipient, derr)
					} else {
						r.log.Printf("recipient deactivated recipient=%s subs=%d: %v", sub.Recipient, n, err)
					}
					if r.cfg.OnRecipientDeactivated != nil {
						r.cfg.OnRecipientDeactivated(sub.Recipient)
					}
					r.invokeOnError(fmt.Errorf(
						"수신자 %s 에게 보낼 수 없어 구독 %d건을 껐다(다시 받으려면 그쪽에서 /start): %w",
						sub.Recipient, n, err))
					continue
				}
				sendFailed = true
				lastSendErr = err
				continue
			}
			matched = true
			if err := r.cfg.Store.MarkSent(sub.ID, item.ID); err != nil {
				// ⚠발송은 이미 성공했다. 여기서 sendFailed 를 세우면 다음 폴에서
				//   IsSent 가 여전히 false 라 **같은 알림이 다시 간다.** 그래서
				//   재시도하지 않고 사람에게 올린다 — 이 기록이 깨지는 것이 곧
				//   중복 발송의 원인이라 로그 한 줄로 넘길 일이 아니다.
				r.log.Printf("mark sent error sub=%s item=%s: %v", sub.ID, item.ID, err)
				r.invokeOnError(fmt.Errorf(
					"발송 기록 실패 — 중복 발송 위험 sub=%s item=%s: %w", sub.ID, item.ID, err))
			}
		}

		if matched && !alreadyDelivered && r.cfg.OnItemMatched != nil {
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
		// ⚠ 훅 실패도 발송 실패와 같은 유예·상한 경로를 탄다(카운터 공유). 재시도
		//   폴의 중복 발송은 IsSent 가, 훅 중복은 위 bot_hook_done 이 막는다.
		if sendFailed || hookErr != nil {
			reason := lastSendErr
			if reason == nil {
				reason = fmt.Errorf("OnNewItem hook: %w", hookErr)
			}
			attempts, err := r.cfg.Store.RecordSendFailure(itemSource(item), item.ID, fmt.Sprint(reason))
			if err != nil {
				r.log.Printf("record send failure error: %v", err)
			}
			if attempts < maxSendAttempts {
				r.log.Printf("send retry pending item=%s attempts=%d/%d", item.ID, attempts, maxSendAttempts)
				continue // seen 처리하지 않는다 → 다음 폴에서 재시도
			}
			r.log.Printf("send gave up item=%s after %d attempts: %v", item.ID, attempts, reason)
			r.invokeOnError(fmt.Errorf("발송/훅 %d회 실패로 포기 item=%s: %w", attempts, item.ID, reason))
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
