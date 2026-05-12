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

// Name returns a stable composite name so bot_seen keys do not change
// when sub-sources are reordered. Sub-source identity is still visible
// via the per-source error wrapping in Fetch.
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

	var failures []error
	successCount := 0
	for r := range results {
		if r.err == nil {
			successCount++
			continue
		}
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
	}
	return nil
}
