package control

import (
	"context"
	"errors"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// lostLogin is a provider login whose handshake disappears, the way it does
// when the process restarts: polling reports the login was never started.
type lostLogin struct{ restarted int }

func (l *lostLogin) StartLogin(context.Context, string) (providers.LoginSession, error) {
	l.restarted++
	return providers.LoginSession{AuthURL: "https://provider.example/auth2"}, nil
}

func (l *lostLogin) PollLogin(context.Context, string) (bool, string, error) {
	return false, "", errors.New("login not started for account acc_x")
}

// A poll whose handshake was lost must report a restartable state, not surface
// the provider-internal error, and must not credit anything.
func TestPollSessionReportsLostHandshakeAsRestartable(t *testing.T) {
	var hits int32
	site := creditSite(t, &hits)
	login := &lostLogin{}
	donations, _, _ := donationHarness(t, &fakeLogin{}, site)
	donations.accounts.Providers.Register(providers.Adapter{ID: "workbuddy", Login: login})

	session, err := donations.StartSession(context.Background(), DonationStart{
		Format: "workbuddy-oauth-v1", NewAPIUserID: 7,
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	polled, err := donations.PollSession(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("a lost handshake must not be a hard error: %v", err)
	}
	if polled.Status != "pending" || polled.Credited {
		t.Fatalf("polled = %+v, want pending and uncredited", polled)
	}
	if polled.Message == "" {
		t.Fatal("the restartable state must be explained to the caller")
	}
	if hits != 0 {
		t.Fatalf("nothing may be credited, hits=%d", hits)
	}
}

// Restarting a pending round re-opens the provider login on the same account.
func TestRestartSessionReopensLogin(t *testing.T) {
	login := &lostLogin{}
	donations, store, _ := donationHarness(t, &fakeLogin{}, nil)
	donations.accounts.Providers.Register(providers.Adapter{ID: "workbuddy", Login: login})

	session, err := donations.StartSession(context.Background(), DonationStart{
		Format: "workbuddy-oauth-v1", NewAPIUserID: 7,
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	before := login.restarted

	restarted, err := donations.RestartSession(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("RestartSession: %v", err)
	}
	if login.restarted != before+1 {
		t.Fatal("restart must re-run the provider login")
	}
	if restarted.AuthURL != "https://provider.example/auth2" {
		t.Fatalf("auth url = %q, want the fresh one", restarted.AuthURL)
	}
	// It must reuse the same account rather than creating a second one.
	if len(store.accounts) != 1 {
		t.Fatalf("restart must not create another account, have %d", len(store.accounts))
	}
	if restarted.AccountID != session.AccountID {
		t.Fatal("restart must reuse the same account")
	}
}

// Restarting a finished round is a no-op, so a settled reward is never re-run.
func TestRestartSessionIgnoresSettled(t *testing.T) {
	var hits int32
	site := creditSite(t, &hits)
	login := &fakeLogin{authURL: "https://provider.example/auth"}
	donations, _, _ := donationHarness(t, login, site)
	session, _ := donations.StartSession(context.Background(), DonationStart{
		Format: "workbuddy-oauth-v1", NewAPIUserID: 7,
	})
	login.finish("login complete")
	if _, err := donations.PollSession(context.Background(), session.ID); err != nil {
		t.Fatalf("PollSession: %v", err)
	}
	if _, err := donations.RestartSession(context.Background(), session.ID); err != nil {
		t.Fatalf("RestartSession: %v", err)
	}
	if hits != 1 {
		t.Fatalf("a settled round must not be re-run, hits=%d", hits)
	}
}
