package executor

import (
	"testing"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
)

// A provider's own RetryAfter must obey the same cap as an HTTP retry hint.
//
// WorkBuddy reports the usage window's absolute reset time as its retry hint
// ("will reset at <timestamp>"), which can be hours away. Taking it at face
// value parked accounts for 15 hours after a SINGLE failure (backoff level 0)
// even though they were still serving -- and it skipped both the retry-after cap
// and the backoff ceiling, because those guard the HTTP header, not this field.
func TestProviderRetryAfterIsCapped(t *testing.T) {
	got := ClassifyError(&providers.Error{
		Kind:       accounts.KindRateLimit,
		Status:     429,
		Message:    "usage limit",
		RetryAfter: 15 * time.Hour,
	})
	if got.Kind != accounts.KindRateLimit {
		t.Fatalf("kind = %s, want rate_limit", got.Kind)
	}
	if got.Cooldown > maxRetryAfter {
		t.Fatalf("cooldown = %v, must not exceed the retry-after cap %v", got.Cooldown, maxRetryAfter)
	}
	if got.Cooldown <= 0 {
		t.Fatalf("cooldown = %v, want a positive wait", got.Cooldown)
	}
	if got.RetryAfter > maxRetryAfter {
		t.Fatalf("retry_after = %v, must not exceed %v", got.RetryAfter, maxRetryAfter)
	}
}

// A short, plausible hint is still honoured (not flattened to the cap or the
// 30-second floor).
func TestProviderShortRetryAfterIsHonoured(t *testing.T) {
	got := ClassifyError(&providers.Error{
		Kind: accounts.KindRateLimit, Status: 429, Message: "slow down",
		RetryAfter: 5 * time.Minute,
	})
	if got.Cooldown != 5*time.Minute {
		t.Fatalf("cooldown = %v, want 5m", got.Cooldown)
	}
}

// A tiny hint is lifted to the 30-second floor rather than hammering upstream.
func TestProviderTinyRetryAfterHitsFloor(t *testing.T) {
	got := ClassifyError(&providers.Error{
		Kind: accounts.KindRateLimit, Status: 429, Message: "slow down",
		RetryAfter: time.Second,
	})
	if got.Cooldown != 30*time.Second {
		t.Fatalf("cooldown = %v, want the 30s floor", got.Cooldown)
	}
}

// The 4011 hard-rate-limit default keeps working when the provider gives no hint.
func TestProvider4011DefaultUnchanged(t *testing.T) {
	got := ClassifyError(&providers.Error{
		Kind: accounts.KindRateLimit, Status: 429, Code: "4011", Message: "hard rate limit",
	})
	if got.Cooldown != 5*time.Minute {
		t.Fatalf("cooldown = %v, want 5m", got.Cooldown)
	}
}

// A quota-kind provider error is a hard park until local midnight; the cap is a
// rate-limit concern and must not shorten it.
func TestQuotaKindIsNotCappedByRetryAfter(t *testing.T) {
	got := ClassifyError(&providers.Error{
		Kind: accounts.KindQuota, Status: 429, Message: "plan exhausted",
		RetryAfter: time.Minute,
	})
	if got.Kind != accounts.KindQuota {
		t.Fatalf("kind = %s, want quota", got.Kind)
	}
	if got.Cooldown > maxRetryAfter {
		// Quota uses next-local-midnight, which is by design longer than the cap.
		return
	}
	t.Fatalf("a quota park should not be squeezed to the rate-limit cap: %v", got.Cooldown)
}
