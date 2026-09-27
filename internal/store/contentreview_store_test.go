package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
)

// ListRecentAttempts returns the newest attempts first and honors its limit, so
// the content-review monitor sees the last N calls.
func TestListRecentAttemptsOrdersNewestFirst(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	account, err := store.Create(ctx, accounts.CreateAccount{Name: "N", Provider: "workbuddy", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertRequestLog(ctx, accounts.RequestLog{ID: "req1", Status: "error"}); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC()
	for i := 0; i < 5; i++ {
		err := store.InsertRequestAttempt(ctx, accounts.RequestAttempt{
			ID: accounts.NewAttemptID(), RequestID: "req1", AttemptIndex: i, AccountID: account.ID,
			StartedAt: base.Add(time.Duration(i) * time.Second),
			Status:    accounts.AttemptStatusError, ErrorKind: accounts.KindInvalidRequest,
			ErrorMessage: `{"code":11140,"msg":"request illegal"}`,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.ListRecentAttempts(ctx, account.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d attempts, want 3 (limit)", len(got))
	}
	// Newest first: attempt_index 4, then 3, then 2.
	for i, want := range []int{4, 3, 2} {
		if got[i].AttemptIndex != want {
			t.Fatalf("got[%d].AttemptIndex = %d, want %d", i, got[i].AttemptIndex, want)
		}
	}
}

// Contribution provenance round-trips through create and read.
func TestAccountContributionProvenanceRoundTrips(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	created, err := store.Create(ctx, accounts.CreateAccount{
		Name: "donated", Provider: "workbuddy", Region: "cn", Enabled: true,
		ContributedBy: 42, ContributedProvider: "workbuddy",
		ContributedRegion: "cn", ContributedFormat: "workbuddy-oauth-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	fetched, err := store.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fetched.ContributedBy != 42 ||
		fetched.ContributedProvider != "workbuddy" ||
		fetched.ContributedRegion != "cn" ||
		fetched.ContributedFormat != "workbuddy-oauth-v1" {
		t.Fatalf("provenance did not round-trip: %+v", fetched)
	}

	// An operator-created account carries none.
	plain, err := store.Create(ctx, accounts.CreateAccount{Name: "plain", Provider: "qoder", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, plain.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ContributedBy != 0 || got.ContributedProvider != "" {
		t.Fatalf("an operator account must not look contributed: %+v", got)
	}
}

// The monitor stamps a reason through Update; it must persist.
func TestUpdateStampsLastErrorReason(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "qoder.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	account, err := store.Create(ctx, accounts.CreateAccount{Name: "B", Provider: "workbuddy", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	off := false
	kind := accounts.KindInvalidRequest
	reason := "auto-disabled: last 10 calls all rejected"
	if err := store.Update(ctx, account.ID, accounts.UpdateAccount{
		Enabled: &off, LastErrorKind: &kind, LastError: &reason,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled {
		t.Fatal("account must be disabled")
	}
	if got.LastError != reason || got.LastErrorKind != accounts.KindInvalidRequest {
		t.Fatalf("reason not persisted: %q / %q", got.LastError, got.LastErrorKind)
	}
}
