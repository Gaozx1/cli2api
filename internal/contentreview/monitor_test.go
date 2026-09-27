package contentreview

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
)

type fakeStore struct {
	accounts  []accounts.Account
	attempts  map[string][]accounts.RequestAttempt
	listErr   error
	attemptEr error
}

func (f *fakeStore) List(context.Context) ([]accounts.Account, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.accounts, nil
}

func (f *fakeStore) ListRecentAttempts(_ context.Context, accountID string, limit int) ([]accounts.RequestAttempt, error) {
	if f.attemptEr != nil {
		return nil, f.attemptEr
	}
	items := f.attempts[accountID]
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

type fakeDisabler struct {
	disabled map[string]accounts.UpdateAccount
}

func (f *fakeDisabler) Update(_ context.Context, id string, input accounts.UpdateAccount) error {
	if f.disabled == nil {
		f.disabled = map[string]accounts.UpdateAccount{}
	}
	f.disabled[id] = input
	return nil
}

const reviewBody = `{"code":11140,"msg":"request illegal","displayMsg":{"en":"Content failed safety review. Please revise it"}}`

func failure() accounts.RequestAttempt {
	return accounts.RequestAttempt{Status: accounts.AttemptStatusError, ErrorKind: accounts.KindInvalidRequest, ErrorMessage: reviewBody}
}

func success() accounts.RequestAttempt {
	return accounts.RequestAttempt{Status: accounts.AttemptStatusOK}
}

func newestFirst(items ...accounts.RequestAttempt) []accounts.RequestAttempt { return items }

func wb(id string) accounts.Account {
	return accounts.Account{ID: id, Name: id, Provider: "workbuddy", Enabled: true, CreatedAt: time.Now()}
}

func sweep(t *testing.T, store *fakeStore) (*fakeDisabler, int) {
	t.Helper()
	disabler := &fakeDisabler{}
	n, err := New(store, disabler).Sweep(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	return disabler, n
}

// The rule: ten consecutive content-review failures with no success disables the
// account.
func TestTenConsecutiveRejectionsDisablesAccount(t *testing.T) {
	store := &fakeStore{
		accounts: []accounts.Account{wb("acc1")},
		attempts: map[string][]accounts.RequestAttempt{"acc1": newestFirst(
			failure(), failure(), failure(), failure(), failure(),
			failure(), failure(), failure(), failure(), failure(),
		)},
	}
	disabler, n := sweep(t, store)
	if n != 1 {
		t.Fatalf("disabled %d accounts, want 1", n)
	}
	got, ok := disabler.disabled["acc1"]
	if !ok {
		t.Fatal("acc1 was not disabled")
	}
	if got.Enabled == nil || *got.Enabled {
		t.Fatalf("Enabled = %v, want false", got.Enabled)
	}
	if got.LastError == nil || got.LastErrorKind == nil {
		t.Fatalf("the reason must be stamped: %+v", got)
	}
}

// A single success inside the last ten calls means the account is not banned.
func TestOneSuccessInWindowSparesAccount(t *testing.T) {
	store := &fakeStore{
		accounts: []accounts.Account{wb("acc1")},
		attempts: map[string][]accounts.RequestAttempt{"acc1": newestFirst(
			failure(), failure(), success(), failure(), failure(),
			failure(), failure(), failure(), failure(), failure(),
		)},
	}
	disabler, n := sweep(t, store)
	if n != 0 {
		t.Fatalf("disabled %d, want 0", n)
	}
	if _, ok := disabler.disabled["acc1"]; ok {
		t.Fatal("a window containing a success must not disable the account")
	}
}

// Fewer than ten calls is not enough evidence.
func TestShortHistoryIsNotEnough(t *testing.T) {
	store := &fakeStore{
		accounts: []accounts.Account{wb("acc1")},
		attempts: map[string][]accounts.RequestAttempt{"acc1": newestFirst(
			failure(), failure(), failure(), failure(), failure(),
			failure(), failure(), failure(), failure(),
		)},
	}
	if _, n := sweep(t, store); n != 0 {
		t.Fatalf("disabled %d, want 0 (only 9 attempts)", n)
	}
}

// Other providers keep their screening rejections request-level: a run of them
// must not cost pool capacity.
func TestOtherProvidersAreIgnored(t *testing.T) {
	acct := wb("acc1")
	acct.Provider = "trae"
	store := &fakeStore{
		accounts: []accounts.Account{acct},
		attempts: map[string][]accounts.RequestAttempt{"acc1": newestFirst(
			failure(), failure(), failure(), failure(), failure(),
			failure(), failure(), failure(), failure(), failure(),
		)},
	}
	if _, n := sweep(t, store); n != 0 {
		t.Fatalf("disabled %d trae accounts, want 0", n)
	}
}

// An already-disabled account is not re-disabled or re-stamped.
func TestAlreadyDisabledIsSkipped(t *testing.T) {
	acct := wb("acc1")
	acct.Enabled = false
	store := &fakeStore{
		accounts: []accounts.Account{acct},
		attempts: map[string][]accounts.RequestAttempt{"acc1": newestFirst(
			failure(), failure(), failure(), failure(), failure(),
			failure(), failure(), failure(), failure(), failure(),
		)},
	}
	if _, n := sweep(t, store); n != 0 {
		t.Fatalf("disabled %d, want 0", n)
	}
}

// A different failure kind in the window must not be mistaken for a content ban.
func TestDifferentFailureKindSparesAccount(t *testing.T) {
	other := accounts.RequestAttempt{Status: accounts.AttemptStatusError, ErrorKind: accounts.KindRateLimit, ErrorMessage: `{"code":4011,"message":"hard rate limit"}`}
	store := &fakeStore{
		accounts: []accounts.Account{wb("acc1")},
		attempts: map[string][]accounts.RequestAttempt{"acc1": newestFirst(
			failure(), failure(), failure(), failure(), failure(),
			failure(), failure(), failure(), failure(), other,
		)},
	}
	if _, n := sweep(t, store); n != 0 {
		t.Fatalf("disabled %d, want 0", n)
	}
}

// The rule is stateless: the same history evaluated twice is idempotent, so a
// restart cannot double-apply.
func TestSweepIsIdempotent(t *testing.T) {
	store := &fakeStore{
		accounts: []accounts.Account{wb("acc1")},
		attempts: map[string][]accounts.RequestAttempt{"acc1": newestFirst(
			failure(), failure(), failure(), failure(), failure(),
			failure(), failure(), failure(), failure(), failure(),
		)},
	}
	m := New(store, &fakeDisabler{})
	for i := 0; i < 3; i++ {
		if _, err := m.Sweep(context.Background()); err != nil {
			t.Fatalf("sweep %d: %v", i, err)
		}
	}
}

func TestAsksContentReviewMatchesCodeAndProse(t *testing.T) {
	for _, body := range []string{
		`{"code":11140,"msg":"request illegal"}`,
		`{"code": 11140}`,
		`code=11140 something`,
		`{"msg":"Content failed safety review. Please revise it"}`,
		`{"displayMsg":{"zh":"内容未通过安全审核，请调整"}}`,
		`{"displayMsg":{"zh-hant":"內容未通過安全審核，請調整"}}`,
	} {
		if !asksContentReview(body) {
			t.Fatalf("body %q must be recognized as a content-review rejection", body)
		}
	}
	for _, body := range []string{
		`{"code":1005,"message":"plan limit"}`,
		`{"code":4011,"message":"hard rate limit"}`,
		"",
	} {
		if asksContentReview(body) {
			t.Fatalf("body %q must NOT be a content-review rejection", body)
		}
	}
}

func TestSweepPropagatesErrors(t *testing.T) {
	store := &fakeStore{listErr: errors.New("db down")}
	if _, err := New(store, &fakeDisabler{}).Sweep(context.Background()); err == nil {
		t.Fatal("a store failure must surface")
	}
	store = &fakeStore{accounts: []accounts.Account{wb("acc1")}, attemptEr: errors.New("db down")}
	if _, err := New(store, &fakeDisabler{}).Sweep(context.Background()); err == nil {
		t.Fatal("an attempts failure must surface")
	}
}
