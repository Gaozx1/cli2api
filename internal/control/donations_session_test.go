package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
)

// fakeLogin is one controllable provider browser login.
type fakeLogin struct {
	mu       sync.Mutex
	authURL  string
	startErr error
	done     bool
	message  string
	polls    int
}

func (f *fakeLogin) StartLogin(context.Context, string) (providers.LoginSession, error) {
	if f.startErr != nil {
		return providers.LoginSession{}, f.startErr
	}
	return providers.LoginSession{AuthURL: f.authURL, State: "st"}, nil
}

func (f *fakeLogin) PollLogin(context.Context, string) (bool, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polls++
	return f.done, f.message, nil
}

func (f *fakeLogin) finish(message string) {
	f.mu.Lock()
	f.done = true
	f.message = message
	f.mu.Unlock()
}

// donationHarness wires a Donations service to the shared fakes plus a fake
// provider login, so web-auth rounds run without a live provider.
func donationHarness(t *testing.T, login *fakeLogin, site *httptest.Server) (*Donations, *fakeStore, *callLog) {
	t.Helper()
	log := &callLog{}
	store := newFakeStore(log)
	runtime := &fakeRuntime{log: log, store: store}
	accountsSvc := NewAccounts(runtime)
	registry := providers.NewRegistry()
	registry.Register(providers.Adapter{ID: "workbuddy", Login: login})
	accountsSvc.Providers = registry

	donations := &Donations{
		settings: NewSettings(store),
		accounts: accountsSvc,
		sessions: map[string]*DonationSession{},
	}
	if site != nil {
		donations.HTTP = site.Client()
		donations.BaseURL = site.URL
		donations.Token = "test-token"
	}
	return donations, store, log
}

func creditSite(t *testing.T, hit *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*hit++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A started round must not credit anything: the account is not yet authorized.
func TestStartSessionDoesNotCredit(t *testing.T) {
	var hits int32
	site := creditSite(t, &hits)
	login := &fakeLogin{authURL: "https://provider.example/auth"}
	donations, store, _ := donationHarness(t, login, site)

	session, err := donations.StartSession(context.Background(), DonationStart{
		Format: "workbuddy-oauth-v1", NewAPIUserID: 7, CreditUSD: 1,
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if session.Status != "pending" {
		t.Fatalf("status = %q, want pending", session.Status)
	}
	if session.AuthURL != "https://provider.example/auth" {
		t.Fatalf("auth url = %q", session.AuthURL)
	}
	if hits != 0 {
		t.Fatalf("credit must not fire before authorization, hits=%d", hits)
	}
	// The placeholder account must exist but stay disabled until authorized.
	created := store.accounts[session.AccountID]
	if created.ID == "" {
		t.Fatal("placeholder account was not created")
	}
	if created.Enabled {
		t.Fatal("placeholder account must not be enabled before authorization")
	}
}

// Completing authorization enables the account and credits exactly once.
func TestPollSessionCreditsOnCompletion(t *testing.T) {
	var hits int32
	site := creditSite(t, &hits)
	login := &fakeLogin{authURL: "https://provider.example/auth"}
	donations, store, _ := donationHarness(t, login, site)

	session, err := donations.StartSession(context.Background(), DonationStart{
		Format: "workbuddy-oauth-v1", NewAPIUserID: 7,
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	// Still pending: no credit.
	pending, err := donations.PollSession(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("PollSession pending: %v", err)
	}
	if pending.Status != "pending" {
		t.Fatalf("status = %q, want pending", pending.Status)
	}
	if hits != 0 {
		t.Fatalf("pending poll must not credit, hits=%d", hits)
	}

	// Provider login completes.
	login.finish("login complete")
	done, err := donations.PollSession(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("PollSession done: %v", err)
	}
	if done.Status != "credited" || !done.Credited {
		t.Fatalf("settled session = %+v, want credited", done)
	}
	if done.CreditedQuota != 500000 {
		t.Fatalf("credited quota = %d, want 500000", done.CreditedQuota)
	}
	if hits != 1 {
		t.Fatalf("credit hits = %d, want 1", hits)
	}
	if !store.accounts[session.AccountID].Enabled {
		t.Fatal("authorized account must be enabled")
	}

	// Polling again must not credit twice.
	again, err := donations.PollSession(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("PollSession again: %v", err)
	}
	if again.Status != "credited" || hits != 1 {
		t.Fatalf("repeat poll double-credited: status=%q hits=%d", again.Status, hits)
	}
}

// A credit failure after a successful authorization must keep the account and
// report the shortfall, not lose the contribution.
func TestPollSessionKeepsAccountWhenCreditFails(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":false,"message":"boom"}`))
	}))
	defer site.Close()

	login := &fakeLogin{authURL: "https://provider.example/auth"}
	donations, store, _ := donationHarness(t, login, site)
	session, _ := donations.StartSession(context.Background(), DonationStart{
		Format: "workbuddy-oauth-v1", NewAPIUserID: 7,
	})
	login.finish("login complete")

	settled, err := donations.PollSession(context.Background(), session.ID)
	if err != nil {
		t.Fatalf("PollSession: %v", err)
	}
	if settled.Credited {
		t.Fatal("credit must not be reported as successful")
	}
	if settled.CreditError == "" || !strings.Contains(settled.CreditError, "boom") {
		t.Fatalf("credit_error = %q, want the site message", settled.CreditError)
	}
	// The authorized account is valuable and must stay, enabled.
	if !store.accounts[session.AccountID].Enabled {
		t.Fatal("an authorized account must remain enabled even when the credit fails")
	}
}

// A failed provider login start must not leave a placeholder account behind.
func TestStartSessionRemovesAccountWhenLoginFails(t *testing.T) {
	login := &fakeLogin{startErr: errors.New("provider down")}
	donations, store, _ := donationHarness(t, login, nil)

	if _, err := donations.StartSession(context.Background(), DonationStart{
		Format: "workbuddy-oauth-v1", NewAPIUserID: 7,
	}); err == nil {
		t.Fatal("a provider login failure must surface")
	}
	if len(store.accounts) != 0 {
		t.Fatalf("placeholder account left behind: %v", store.accounts)
	}
}

// Web authorization requires the provider to expose a browser login.
func TestStartSessionRejectsProviderWithoutWebAuth(t *testing.T) {
	donations, _, _ := donationHarness(t, &fakeLogin{}, nil)
	// trae has a credential format but no login registered in this harness.
	if _, err := donations.StartSession(context.Background(), DonationStart{
		Format: "trae-oauth-v1", NewAPIUserID: 7,
	}); err == nil {
		t.Fatal("a provider without browser login must be rejected")
	}
}

// Cancelling abandons the round and removes the placeholder account.
func TestCancelSessionRemovesPlaceholder(t *testing.T) {
	login := &fakeLogin{authURL: "https://provider.example/auth"}
	donations, store, _ := donationHarness(t, login, nil)
	session, _ := donations.StartSession(context.Background(), DonationStart{
		Format: "workbuddy-oauth-v1", NewAPIUserID: 7,
	})

	if err := donations.CancelSession(context.Background(), session.ID); err != nil {
		t.Fatalf("CancelSession: %v", err)
	}
	if _, ok := store.accounts[session.AccountID]; ok {
		t.Fatal("cancel must remove the placeholder account")
	}
	if _, err := donations.PollSession(context.Background(), session.ID); err == nil {
		t.Fatal("a cancelled session must not be pollable")
	}
}

// A finished contribution must not be cancellable: the reward is already paid.
func TestCancelSessionRefusesSettled(t *testing.T) {
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
	if err := donations.CancelSession(context.Background(), session.ID); err == nil {
		t.Fatal("a settled contribution must not be cancellable")
	}
}

// An expired pending round is swept along with its placeholder account.
func TestSweepExpiredSessionRemovesAccount(t *testing.T) {
	login := &fakeLogin{authURL: "https://provider.example/auth"}
	donations, store, _ := donationHarness(t, login, nil)
	session, _ := donations.StartSession(context.Background(), DonationStart{
		Format: "workbuddy-oauth-v1", NewAPIUserID: 7,
	})

	// Move the clock past the TTL, then trigger a sweep via a new round.
	base := time.Now()
	donations.clock = func() time.Time { return base.Add(donationSessionTTL + time.Minute) }
	donations.mu.Lock()
	expired := donations.sweepLocked()
	donations.mu.Unlock()
	donations.deleteSweptAccounts(context.Background(), expired)

	if _, ok := donations.sessions[session.ID]; ok {
		t.Fatal("expired session was not swept")
	}
	if _, ok := store.accounts[session.AccountID]; ok {
		t.Fatal("expired session left its placeholder account behind")
	}
}

// A settled session survives the TTL so a finished reward is still reportable.
func TestSweepKeepsSettledSession(t *testing.T) {
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

	base := time.Now()
	donations.clock = func() time.Time { return base.Add(donationSessionTTL + time.Minute) }
	donations.mu.Lock()
	donations.sweepLocked()
	donations.mu.Unlock()

	if _, ok := donations.sessions[session.ID]; !ok {
		t.Fatal("a settled session must not be swept")
	}
}

// The console must advertise which formats support web authorization.
func TestDonationInfoReportsWebAuth(t *testing.T) {
	// The capability comes from the static provider registry, so assert on the
	// real descriptors rather than the harness.
	for _, descriptor := range providers.List() {
		if !descriptor.Capabilities.BrowserLogin {
			continue
		}
		if descriptor.ID == "workbuddy" && !descriptor.SupportsCredentialFormat("workbuddy-oauth-v1") {
			t.Fatal("workbuddy must expose its credential format")
		}
	}
	encoded, _ := json.Marshal(providers.WorkBuddy.Capabilities)
	if !strings.Contains(string(encoded), "browser_login") {
		t.Fatalf("capabilities must expose browser_login: %s", encoded)
	}
}

var _ = accounts.Account{}
