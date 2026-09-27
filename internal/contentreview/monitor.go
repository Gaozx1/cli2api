// Package contentreview disables accounts that an upstream provider has
// effectively banned.
//
// WorkBuddy answers a provider-side content ban with code 11140 ("Content failed
// safety review") to *every* request, regardless of the request's content. The
// classifier cannot see that: 11140 is request-level by shape, so it is recorded
// as invalid_request with no cooldown and no failover, and the account keeps
// being picked and keeps failing. Measured on this deployment: 22 accounts with
// 244-310 errors and 0-4 successes each, all returning 11140 for a benign
// prompt.
//
// The signal is therefore the *pattern* across recent calls, not the shape of
// one error. This monitor reads the call history and, when an account's last N
// attempts all failed with 11140, disables that account.
//
// It is deliberately stateless: every tick re-derives the verdict from the
// attempts table, so a restart cannot lose or double-apply a decision, and an
// account that starts succeeding drops out of the window naturally.
package contentreview

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
)

// Provider is the only provider this rule targets. 11140 is a WorkBuddy code and
// the ban behaviour was measured there; other providers' screening errors are
// genuine request-level rejections and must not cost them pool capacity.
const Provider = "workbuddy"

// ReviewCode is the WorkBuddy envelope code for a content-safety rejection.
const ReviewCode = "11140"

// WindowSize is how many of an account's most recent attempts are examined.
// "The last 10 calls" is the rule the operator asked for.
const WindowSize = 10

// SweepInterval is how often the monitor evaluates the rule. Frequent enough to
// pull a banned account out quickly, slow enough to be free.
const SweepInterval = time.Minute

// AccountStore is the slice of the account store the monitor needs.
type AccountStore interface {
	List(ctx context.Context) ([]accounts.Account, error)
	ListRecentAttempts(ctx context.Context, accountID string, limit int) ([]accounts.RequestAttempt, error)
}

// Disabler turns an account off through the runtime so its process stops too.
type Disabler interface {
	Update(ctx context.Context, id string, input accounts.UpdateAccount) error
}

// AutoDisableReason is the operator-facing explanation stamped on the account.
const AutoDisableReason = "auto-disabled: last 10 calls all rejected by upstream content review (WorkBuddy 11140) - account-level block; re-enable only after the provider lifts it"

// Monitor applies the rule.
type Monitor struct {
	store    AccountStore
	disabler Disabler
	interval time.Duration
	window   int
}

// New builds a monitor. A nil disabler falls back to the store, which is enough
// for tests but leaves a child process running in production; the app wires the
// runtime manager instead.
func New(store AccountStore, disabler Disabler) *Monitor {
	if disabler == nil {
		if asDisabler, ok := store.(Disabler); ok {
			disabler = asDisabler
		}
	}
	return &Monitor{
		store: store, disabler: disabler,
		interval: SweepInterval, window: WindowSize,
	}
}

// RunSweepLoop evaluates the rule until stop closes.
func (m *Monitor) RunSweepLoop(stop <-chan struct{}) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if n, err := m.Sweep(context.Background()); err != nil {
				log.Printf("content review sweep: %v", err)
			} else if n > 0 {
				log.Printf("content review sweep: auto-disabled %d account(s) banned upstream", n)
			}
		}
	}
}

// Sweep applies the rule once and returns how many accounts it disabled.
func (m *Monitor) Sweep(ctx context.Context) (int, error) {
	all, err := m.store.List(ctx)
	if err != nil {
		return 0, fmt.Errorf("list accounts: %w", err)
	}
	disabled := 0
	for _, account := range all {
		if !account.Enabled || accounts.NormalizeProviderFamily(account.Provider) != Provider {
			continue
		}
		attempts, err := m.store.ListRecentAttempts(ctx, account.ID, m.window)
		if err != nil {
			return disabled, fmt.Errorf("attempts for %s: %w", account.ID, err)
		}
		if !banned(attempts, m.window) {
			continue
		}
		off := false
		reason := AutoDisableReason
		kind := accounts.KindInvalidRequest
		if err := m.disabler.Update(ctx, account.ID, accounts.UpdateAccount{
			Enabled:       &off,
			LastErrorKind: &kind,
			LastError:     &reason,
		}); err != nil {
			return disabled, fmt.Errorf("disable %s: %w", account.ID, err)
		}
		log.Printf("content review: disabled account=%s name=%q - last %d attempts all %s",
			account.ID, account.Name, m.window, ReviewCode)
		disabled++
	}
	return disabled, nil
}

// banned reports whether the account's most recent attempts are a full window of
// content-review rejections. Requiring a FULL window means a new account with
// one rejection is not disabled.
func banned(attempts []accounts.RequestAttempt, window int) bool {
	if len(attempts) < window {
		return false
	}
	// ListRecentAttempts returns newest first.
	for _, attempt := range attempts[:window] {
		if !isContentReviewFailure(attempt) {
			return false
		}
	}
	return true
}

func isContentReviewFailure(attempt accounts.RequestAttempt) bool {
	if attempt.Status == accounts.AttemptStatusOK {
		return false
	}
	return asksContentReview(attempt.ErrorMessage)
}

// asksContentReview reports whether an error body carries WorkBuddy's 11140
// content-safety rejection. Matching the code (not the prose) keeps this stable
// across the provider's wording changes; the prose markers are a fallback for
// bodies that omit the code.
func asksContentReview(message string) bool {
	lower := strings.ToLower(message)
	for _, marker := range []string{`"code":`, `"code" :`, "code=", "code: "} {
		if i := strings.Index(lower, marker); i >= 0 {
			rest := strings.TrimLeft(lower[i+len(marker):], " \"")
			if strings.HasPrefix(rest, ReviewCode) {
				return true
			}
		}
	}
	return strings.Contains(lower, "content failed safety review") ||
		strings.Contains(lower, "未通过安全审核") ||
		strings.Contains(lower, "未通過安全審核")
}
