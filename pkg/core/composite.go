package core

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// MultiSource merges multiple sources into one. A single sub-source
// failure does NOT abort the merged fetch — the Runner is contracted
// to dispatch whatever items came back even when err != nil.
//
// Per-source errors are de-duplicated against the last reported
// message so a long-running outage (e.g. a permanently flaky source
// returning the same 404 every poll) does not spam the runner log.
// The first occurrence is always reported; subsequent identical
// errors are suppressed until either the message changes or one
// hour has passed.
type MultiSource struct {
	Sources []Source

	mu       sync.Mutex
	lastErrs map[string]struct {
		msg string
		at  time.Time
	}
}

func NewMultiSource(sources ...Source) *MultiSource {
	return &MultiSource{Sources: sources}
}

// Name 은 하위 소스를 나열한 합성 이름이다.
//
// ⚠ 이 이름을 dedup 키로 쓰면 안 된다. 하위 소스를 켜고 끄면(그리고 순서를 바꿔도)
//
//	값이 바뀌어 seen 이력이 통째로 고아가 된다 — 예전 주석은 "정렬돼 있어 안정적"
//	이라고 적혀 있었지만 실제로는 정렬하지 않는다. Runner 는 이제 Item.Source
//	(하위 소스 이름)를 dedup 키로 쓰므로 이 이름은 로그·식별용으로만 남는다.
func (ms *MultiSource) Name() string {
	if len(ms.Sources) == 0 {
		return "multi-source"
	}
	parts := make([]string, 0, len(ms.Sources))
	for _, s := range ms.Sources {
		parts = append(parts, s.Name())
	}
	return "multi[" + strings.Join(parts, ",") + "]"
}

// Fetch calls Fetch on each sub-source and merges the results. When some
// sources fail, the joined error is returned alongside the partial item
// slice — the Runner treats this as partial success.
func (ms *MultiSource) Fetch(ctx context.Context) ([]Item, error) {
	var allItems []Item
	var errs []error

	for _, s := range ms.Sources {
		items, err := s.Fetch(ctx)
		if err != nil {
			if ms.shouldReport(s.Name(), err.Error()) {
				errs = append(errs, fmt.Errorf("source %s: %w", s.Name(), err))
			}
			// Still merge whatever items the sub-source returned — some
			// scrapers yield partial results before reporting an error.
		}
		// 하위 소스 이름을 스탬프한다 — Runner 가 이 값을 dedup 키로 쓴다.
		// 이미 값이 있으면(중첩 MultiSource) 안쪽 것을 존중한다.
		name := s.Name()
		for i := range items {
			if items[i].Source == "" {
				items[i].Source = name
			}
		}
		allItems = append(allItems, items...)
	}

	if len(errs) > 0 {
		return allItems, errors.Join(errs...)
	}
	return allItems, nil
}

// shouldReport returns true the first time a given (source, message)
// pair is seen, and again only after one hour of repetition. Distinct
// messages always pass through.
func (ms *MultiSource) shouldReport(source, msg string) bool {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if ms.lastErrs == nil {
		ms.lastErrs = make(map[string]struct {
			msg string
			at  time.Time
		})
	}
	prev, ok := ms.lastErrs[source]
	if ok && prev.msg == msg && time.Since(prev.at) < time.Hour {
		return false
	}
	ms.lastErrs[source] = struct {
		msg string
		at  time.Time
	}{msg: msg, at: time.Now()}
	return true
}

// MultiNotifier delivers a message to multiple delivery channels in
// parallel. Send returns nil if at least one sub-notifier succeeded —
// this prevents the Runner from re-sending on the next poll to channels
// that already delivered (i.e., it lets MarkSent advance). Per-channel
// failures are logged and accessible via the returned error only when
// every channel failed.
type MultiNotifier struct {
	Notifiers []Notifier
	// Logger receives per-channel failure notes when some — but not all
	// — channels failed. nil disables (default log package is used).
	Logger *log.Logger

	// OnPartialFailure 는 부분 실패(일부 채널만 실패, Send 는 nil 반환) 때
	// 실패한 채널마다 한 번씩 불린다. 부분 성공이 nil 을 돌려주는 의미론
	// 때문에 ErrPermanentRecipient 가 Runner 에 절대 닿지 않는다 — 봇이 한
	// 채널에서 강퇴돼도(bot was kicked) 다른 채널이 살아 있으면 로그 한 줄
	// 뒤 성공으로 기록돼 몇 달간 무음이 된다. 이 훅으로 그런 실패를 봇의
	// 경보 경로(notifyhub.LogPush, OnError 등)로 표면화한다. 훅 안에서
	// IsPermanentRecipient(err) 로 영구/일시를 구별할 수 있다.
	// nil 이면 기존과 동일하게 로그만 남는다. 모든 채널이 실패하면 Send 가
	// error 를 반환해 Runner 의 정규 경로가 처리하므로 이 훅은 불리지 않는다.
	// 훅 안에서 panic 이 나도 Send 는 죽지 않는다 — 하위 notifier 와 같은
	// recover 로 회수해 로그만 남긴다(경보 훅의 버그가 프로세스를 죽이면 본말전도).
	OnPartialFailure func(name string, err error)
}

func NewMultiNotifier(notifiers ...Notifier) *MultiNotifier {
	return &MultiNotifier{Notifiers: notifiers}
}

func (mn *MultiNotifier) Name() string {
	return "multi-notifier"
}

// Send broadcasts the message to all underlying notifiers.
func (mn *MultiNotifier) Send(ctx context.Context, recipient string, msg Message) error {
	if len(mn.Notifiers) == 0 {
		return errors.New("multi-notifier: no notifiers configured")
	}

	type result struct {
		name string
		err  error
	}
	results := make(chan result, len(mn.Notifiers))

	var wg sync.WaitGroup
	for _, n := range mn.Notifiers {
		wg.Add(1)
		go func(ntf Notifier) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					results <- result{name: ntf.Name(), err: fmt.Errorf("panicked: %v", r)}
				}
			}()
			err := ntf.Send(ctx, recipient, msg)
			results <- result{name: ntf.Name(), err: err}
		}(n)
	}
	wg.Wait()
	close(results)

	var failed []result
	var failures []error
	successCount := 0
	for r := range results {
		if r.err == nil {
			successCount++
			continue
		}
		failed = append(failed, r)
		failures = append(failures, fmt.Errorf("%s: %w", r.name, r.err))
	}

	// All channels failed — surface the error so the Runner does not MarkSent.
	if successCount == 0 {
		return fmt.Errorf("multi-notifier all channels failed: %w", errors.Join(failures...))
	}

	// Partial failure — log but report success so MarkSent advances.
	if len(failures) > 0 {
		joined := errors.Join(failures...)
		if mn.Logger != nil {
			mn.Logger.Printf("multi-notifier partial failure (%d of %d failed): %v",
				len(failures), len(mn.Notifiers), joined)
		} else {
			log.Printf("multi-notifier partial failure (%d of %d failed): %v",
				len(failures), len(mn.Notifiers), joined)
		}
		if mn.OnPartialFailure != nil {
			for _, f := range failed {
				// 채널별로 따로 회수한다 — 앞 채널 훅의 panic 이 뒤 채널
				// 통지를 막지 않도록(하위 notifier goroutine 의 recover 와 대칭).
				func() {
					defer func() {
						if r := recover(); r != nil {
							if mn.Logger != nil {
								mn.Logger.Printf("multi-notifier OnPartialFailure hook panicked (channel %s): %v", f.name, r)
							} else {
								log.Printf("multi-notifier OnPartialFailure hook panicked (channel %s): %v", f.name, r)
							}
						}
					}()
					mn.OnPartialFailure(f.name, f.err)
				}()
			}
		}
	}
	return nil
}
