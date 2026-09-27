package control

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
)

// fakeProber is a provider prober whose liveness answer is controllable, so a
// test can distinguish "the credential is shaped right" from "the account
// actually works".
type fakeProber struct {
	mu       sync.Mutex
	ready    bool
	lastErr  string
	quotaErr error
	probes   int
	quotas   int
}

func (f *fakeProber) Probe(context.Context, string) (providers.AccountHealth, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probes++
	return providers.AccountHealth{Ready: f.ready, UID: "uid-1", LastError: f.lastErr}, nil
}

func (f *fakeProber) Quota(context.Context, string) (*providers.QuotaInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.quotas++
	if f.quotaErr != nil {
		return nil, f.quotaErr
	}
	return &providers.QuotaInfo{}, nil
}

func (f *fakeProber) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.probes, f.quotas
}

// donationModelHarness wires a Donations service with a provider that both
// imports a credential and can prove the account is live, so the liveness and
// dedupe gates can be exercised end to end.
func donationModelHarness(t *testing.T, prober *fakeProber) (*Donations, *fakeStore) {
	t.Helper()
	log := &callLog{}
	store := newFakeStore(log)
	runtime := &fakeRuntime{log: log, store: store}
	accountsSvc := NewAccounts(runtime)
	registry := providers.NewRegistry()
	importer := &fakeDonationImporter{format: "command-key-v1"}
	registry.Register(providers.Adapter{
		ID:         "command",
		Credential: importer,
		Prober:     prober,
	})
	accountsSvc.Providers = registry

	donations := &Donations{
		settings: NewSettings(store),
		accounts: accountsSvc,
		sessions: map[string]*DonationSession{},
	}
	return donations, store
}

// The fingerprint must identify a credential by content, so the same account
// resubmitted cannot be rewarded twice.
func TestCredentialFingerprintIsStable(t *testing.T) {
	a := []byte(`{"api_key":"user_abc","email":"x@example.com"}`)
	b := []byte(`{"email":"x@example.com","api_key":"user_abc"}`)
	if donationCredentialFingerprint("command-key-v1", a) != donationCredentialFingerprint("command-key-v1", b) {
		t.Fatal("key order must not change the fingerprint")
	}
	// A different credential must differ.
	c := []byte(`{"api_key":"user_zzz","email":"x@example.com"}`)
	if donationCredentialFingerprint("command-key-v1", a) == donationCredentialFingerprint("command-key-v1", c) {
		t.Fatal("different credentials must not collide")
	}
	// A host-override resubmission is the same credential, not a new one: the
	// fingerprint is computed after the strip.
	d := []byte(`{"api_key":"user_abc","email":"x@example.com","base_url":"https://evil.example"}`)
	if donationCredentialFingerprint("command-key-v1", a) != donationCredentialFingerprint("command-key-v1", d) {
		t.Fatal("adding a host override must not make it look like a new credential")
	}
	// The same payload under a different format is a different claim.
	if donationCredentialFingerprint("command-key-v1", a) == donationCredentialFingerprint("trae-oauth-v1", a) {
		t.Fatal("format must be part of the fingerprint")
	}
}

// A credential that is not live must not be rewarded, and must not be left in
// the pool either.
func TestSubmitRefusesNotLiveAccount(t *testing.T) {
	prober := &fakeProber{ready: true, quotaErr: errNotLive}
	donations, store := donationModelHarness(t, prober)
	var hits int32
	site := creditSite(t, &hits)
	donations.HTTP = site.Client()
	donations.BaseURL = site.URL
	donations.Token = "t"

	_, err := donations.Submit(context.Background(), DonationRequest{
		Format:       "command-key-v1",
		NewAPIUserID: 7,
		Credential:   []byte(`{"api_key":"user_x"}`),
	})
	if err == nil {
		t.Fatal("an account that fails its provider check must be refused")
	}
	if !strings.Contains(err.Error(), "could not be verified") {
		t.Fatalf("err = %v, want a verification failure", err)
	}
	if hits != 0 {
		t.Fatalf("a not-live account must never be credited, hits=%d", hits)
	}
	// The imported copy must be removed again, not left as a dead pool entry.
	if len(store.accounts) != 0 {
		t.Fatalf("a refused account must not stay in the pool: %v", store.accounts)
	}
}

// A probe that positively reports not-ready is authoritative, even if the quota
// call would have succeeded.
func TestSubmitRefusesProbeNotReady(t *testing.T) {
	prober := &fakeProber{ready: false, lastErr: "invalid token"}
	donations, _ := donationModelHarness(t, prober)
	var hits int32
	site := creditSite(t, &hits)
	donations.HTTP = site.Client()
	donations.BaseURL = site.URL
	donations.Token = "t"

	_, err := donations.Submit(context.Background(), DonationRequest{
		Format:       "command-key-v1",
		NewAPIUserID: 7,
		Credential:   []byte(`{"api_key":"user_x"}`),
	})
	if err == nil {
		t.Fatal("a probe reporting not-ready must be refused")
	}
	if hits != 0 {
		t.Fatalf("nothing may be credited, hits=%d", hits)
	}
}

// A live account is credited, and the liveness proof is an actual provider call.
func TestSubmitCreditsLiveAccount(t *testing.T) {
	prober := &fakeProber{ready: true}
	donations, store := donationModelHarness(t, prober)
	var hits int32
	site := creditSite(t, &hits)
	donations.HTTP = site.Client()
	donations.BaseURL = site.URL
	donations.Token = "t"

	result, err := donations.Submit(context.Background(), DonationRequest{
		Format:       "command-key-v1",
		NewAPIUserID: 7,
		Credential:   []byte(`{"api_key":"user_x"}`),
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !result.Credited || hits != 1 {
		t.Fatalf("a live account must be credited once: %+v hits=%d", result, hits)
	}
	if _, quotas := prober.counts(); quotas == 0 {
		t.Fatal("liveness must involve a real provider call (Quota)")
	}
	if len(store.accounts) != 1 {
		t.Fatalf("the live account must stay in the pool, have %d", len(store.accounts))
	}
}

// The same credential submitted twice must be refused the second time, so one
// account cannot be paid for repeatedly.
func TestSubmitRefusesDuplicateCredential(t *testing.T) {
	prober := &fakeProber{ready: true}
	donations, _ := donationModelHarness(t, prober)
	var hits int32
	site := creditSite(t, &hits)
	donations.HTTP = site.Client()
	donations.BaseURL = site.URL
	donations.Token = "t"

	payload := []byte(`{"api_key":"user_dup"}`)
	if _, err := donations.Submit(context.Background(), DonationRequest{
		Format: "command-key-v1", NewAPIUserID: 7, Credential: payload,
	}); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	// Resubmit under a different name, with a host override for good measure:
	// it is still the same credential.
	_, err := donations.Submit(context.Background(), DonationRequest{
		Format:       "command-key-v1",
		Name:         "second try",
		NewAPIUserID: 8,
		Credential:   []byte(`{"api_key":"user_dup","base_url":"https://evil.example"}`),
	})
	if err == nil {
		t.Fatal("a duplicate credential must be refused")
	}
	if !strings.Contains(err.Error(), "already been contributed") {
		t.Fatalf("err = %v, want an already-contributed refusal", err)
	}
	if hits != 1 {
		t.Fatalf("the duplicate must not be paid, hits=%d", hits)
	}
}

// One New API user cannot farm the endpoint with many credentials.
func TestSubmitEnforcesPerUserRewardLimit(t *testing.T) {
	prober := &fakeProber{ready: true}
	donations, _ := donationModelHarness(t, prober)
	var hits int32
	site := creditSite(t, &hits)
	donations.HTTP = site.Client()
	donations.BaseURL = site.URL
	donations.Token = "t"

	for i := 0; i < donationMaxRewardsPerUser; i++ {
		payload := []byte(`{"api_key":"user_` + string(rune('a'+i)) + `"}`)
		if _, err := donations.Submit(context.Background(), DonationRequest{
			Format: "command-key-v1", NewAPIUserID: 7, Credential: payload,
		}); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	// One more distinct credential for the same user must be refused.
	_, err := donations.Submit(context.Background(), DonationRequest{
		Format: "command-key-v1", NewAPIUserID: 7, Credential: []byte(`{"api_key":"user_extra"}`),
	})
	if err == nil {
		t.Fatal("the per-user limit must be enforced")
	}
	if !strings.Contains(err.Error(), "already received") {
		t.Fatalf("err = %v, want a reward-limit refusal", err)
	}
	if int(hits) != donationMaxRewardsPerUser {
		t.Fatalf("hits = %d, want %d", hits, donationMaxRewardsPerUser)
	}
}

// The ledger must survive a restart, or every claim re-opens.
func TestRewardLedgerSurvivesRestart(t *testing.T) {
	prober := &fakeProber{ready: true}
	donations, store := donationModelHarness(t, prober)
	var hits int32
	site := creditSite(t, &hits)
	donations.HTTP = site.Client()
	donations.BaseURL = site.URL
	donations.Token = "t"

	payload := []byte(`{"api_key":"user_persist"}`)
	if _, err := donations.Submit(context.Background(), DonationRequest{
		Format: "command-key-v1", NewAPIUserID: 7, Credential: payload,
	}); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	if _, ok := store.secrets[donationLedgerSecret]; !ok {
		t.Fatal("the ledger must be persisted")
	}

	// Simulate a restart: a fresh service over the same store.
	restarted := &Donations{
		settings: NewSettings(store),
		accounts: donations.accounts,
	}
	restarted.HTTP = site.Client()
	restarted.BaseURL = site.URL
	restarted.Token = "t"

	_, err := restarted.Submit(context.Background(), DonationRequest{
		Format: "command-key-v1", NewAPIUserID: 7, Credential: payload,
	})
	if err == nil {
		t.Fatal("a restart must not re-open an already-paid claim")
	}
	if hits != 1 {
		t.Fatalf("hits = %d, want 1 (no second payout after restart)", hits)
	}
}

// A provider with no prober cannot be verified, so it must not be rewarded:
// paying for an account nobody can check is the failure mode being prevented.
func TestSubmitRefusesUnverifiableProvider(t *testing.T) {
	log := &callLog{}
	store := newFakeStore(log)
	runtime := &fakeRuntime{log: log, store: store}
	accountsSvc := NewAccounts(runtime)
	registry := providers.NewRegistry()
	// A credential importer but no prober: the credential imports, verification
	// cannot happen.
	registry.Register(providers.Adapter{
		ID:         "workbuddy",
		Credential: &fakeDonationImporter{format: "workbuddy-oauth-v1"},
	})
	accountsSvc.Providers = registry

	donations := &Donations{
		settings: NewSettings(store),
		accounts: accountsSvc,
		sessions: map[string]*DonationSession{},
	}
	var hits int32
	site := creditSite(t, &hits)
	donations.HTTP = site.Client()
	donations.BaseURL = site.URL
	donations.Token = "t"

	_, err := donations.Submit(context.Background(), DonationRequest{
		Format:       "workbuddy-oauth-v1",
		NewAPIUserID: 7,
		Credential:   []byte(`{"access_token":"t"}`),
	})
	if err == nil {
		t.Fatal("a provider that cannot verify an account must not be rewarded")
	}
	if hits != 0 {
		t.Fatalf("nothing may be credited, hits=%d", hits)
	}
}

var errNotLive = &providerCheckError{"the account is not usable"}

type providerCheckError struct{ msg string }

func (e *providerCheckError) Error() string { return e.msg }

var _ = json.Marshal
var _ = accounts.Account{}
