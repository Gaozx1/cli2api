package control

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// fakeDonationImporter records exactly what a provider credential importer was
// handed, so a test can assert on the payload that reached it.
type fakeDonationImporter struct {
	format string
	mu     sync.Mutex
	got    []byte
}

func (f *fakeDonationImporter) Validate([]byte) error { return nil }
func (f *fakeDonationImporter) Format() string        { return f.format }

func (f *fakeDonationImporter) PrepareImport(payload []byte) (providers.CredentialImport, error) {
	f.mu.Lock()
	f.got = append([]byte(nil), payload...)
	f.mu.Unlock()
	return providers.CredentialImport{Payload: payload, Ready: true}, nil
}

func (f *fakeDonationImporter) payload() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.got...)
}

func donationImportHarness(t *testing.T, format string, importer *fakeDonationImporter) (*Donations, *fakeStore) {
	t.Helper()
	log := &callLog{}
	store := newFakeStore(log)
	runtime := &fakeRuntime{log: log, store: store}
	accountsSvc := NewAccounts(runtime)
	registry := providers.NewRegistry()
	// A prober is registered too: submitting a credential now requires proving
	// the account is live, and these tests are about pricing and the host strip,
	// not about the liveness gate (which has its own tests).
	registry.Register(providers.Adapter{
		ID:         "workbuddy",
		Credential: importer,
		Prober:     &fakeProber{ready: true},
		Models:     &fakeModels{},
	})
	accountsSvc.Providers = registry
	return &Donations{
		settings: NewSettings(store),
		accounts: accountsSvc,
		sessions: map[string]*DonationSession{},
	}, store
}

// A contributed credential must not be able to name the host the provider talks
// to: once the account is enabled it carries other people's requests.
func TestStripDonationCredentialHostsRemovesUpstreamOverrides(t *testing.T) {
	payload := []byte(`{"access_token":"keep","base_url":"http://evil.example",
		"auth":{"api_host":"http://evil.example","accessToken":"keep2"}}`)

	stripped, err := stripDonationCredentialHosts(payload)
	if err != nil {
		t.Fatalf("strip: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(stripped, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := doc["base_url"]; ok {
		t.Fatal("base_url must be stripped")
	}
	auth, _ := doc["auth"].(map[string]any)
	if _, ok := auth["api_host"]; ok {
		t.Fatal("a nested api_host must be stripped too")
	}
	if doc["access_token"] != "keep" || auth["accessToken"] != "keep2" {
		t.Fatalf("unrelated fields must survive: %s", stripped)
	}
}

// The caller does not price its own payout: credit_usd is accepted for
// compatibility and ignored, and the amount that reaches the donation site is
// the server's.
func TestSubmitIgnoresCallerSuppliedReward(t *testing.T) {
	var gotBody string
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer site.Close()

	importer := &fakeDonationImporter{format: "workbuddy-oauth-v1"}
	donations, _ := donationImportHarness(t, "workbuddy-oauth-v1", importer)
	donations.HTTP = site.Client()
	donations.BaseURL = site.URL
	donations.Token = "t"

	result, err := donations.Submit(context.Background(), DonationRequest{
		Format:       "workbuddy-oauth-v1",
		NewAPIUserID: 7,
		CreditUSD:    1000000,
		Credential:   json.RawMessage(`{"access_token":"t"}`),
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if result.CreditedUSD != donationDefaultUSD {
		t.Fatalf("credited_usd = %v, want the server amount %v", result.CreditedUSD, donationDefaultUSD)
	}
	if result.CreditedQuota != QuotaForUSD(donationDefaultUSD) {
		t.Fatalf("credited_quota = %d, want %d", result.CreditedQuota, QuotaForUSD(donationDefaultUSD))
	}
	want := fmt.Sprintf(`"value":%d`, QuotaForUSD(donationDefaultUSD))
	if !strings.Contains(gotBody, want) {
		t.Fatalf("the credit request did not carry the server amount %s: %s", want, gotBody)
	}
}

// The web-authorization entry point ignores it too, and reports the server
// amount back to the page.
func TestStartSessionIgnoresCallerSuppliedReward(t *testing.T) {
	login := &fakeLogin{authURL: "https://provider.example/auth"}
	donations, _, _ := donationHarness(t, login, nil)

	session, err := donations.StartSession(context.Background(), DonationStart{
		Format:       "workbuddy-oauth-v1",
		NewAPIUserID: 7,
		CreditUSD:    1000000,
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if session.CreditUSD != donationDefaultUSD {
		t.Fatalf("session credit_usd = %v, want %v", session.CreditUSD, donationDefaultUSD)
	}
}

// A pasted credential is unvalidated input: the account it creates must wait for
// an operator, and the upstream override must never reach the importer.
func TestSubmitImportsContributedAccountDisabled(t *testing.T) {
	var hits atomic.Int32
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer site.Close()

	importer := &fakeDonationImporter{format: "workbuddy-oauth-v1"}
	donations, store := donationImportHarness(t, "workbuddy-oauth-v1", importer)
	donations.HTTP = site.Client()
	donations.BaseURL = site.URL
	donations.Token = "t"

	result, err := donations.Submit(context.Background(), DonationRequest{
		Format:       "workbuddy-oauth-v1",
		NewAPIUserID: 7,
		CreditUSD:    1,
		Credential:   json.RawMessage(`{"access_token":"t","base_url":"http://evil.example"}`),
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !result.Credited {
		t.Fatalf("a healthy site must pay out, credit_error=%q", result.CreditError)
	}
	if hits.Load() != 1 {
		t.Fatalf("credit calls = %d, want 1", hits.Load())
	}
	account := store.accounts[result.AccountID]
	if account.ID == "" {
		t.Fatalf("contributed account %q was not stored", result.AccountID)
	}
	if account.Enabled {
		t.Fatal("a contributed credential must not enable itself")
	}
	if strings.Contains(string(importer.payload()), "evil.example") {
		t.Fatalf("an upstream override reached the importer: %s", importer.payload())
	}
}

// Two polls that observe the same completed login must still pay out once: the
// settle path is claimed under the lock, not inferred from a pre-lock read.
func TestPollSessionCreditsOnceUnderConcurrency(t *testing.T) {
	var hits atomic.Int32
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer site.Close()

	login := &fakeLogin{authURL: "https://provider.example/auth"}
	donations, _, _ := donationHarness(t, login, site)

	session, err := donations.StartSession(context.Background(), DonationStart{
		Format:       "workbuddy-oauth-v1",
		NewAPIUserID: 7,
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	login.finish("authorized")

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = donations.PollSession(context.Background(), session.ID)
		}()
	}
	wg.Wait()

	if got := hits.Load(); got != 1 {
		t.Fatalf("credit fired %d times, want exactly 1", got)
	}
}

// A session id is all that separates an anonymous caller from someone else's
// in-flight round, so it must not be derived from the clock.
func TestDonationSessionIDsAreUnpredictable(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		id := donationSessionID()
		if !strings.HasPrefix(id, "don_") {
			t.Fatalf("id %q is missing its prefix", id)
		}
		if len(id) < 20 {
			t.Fatalf("id %q is too short to be unguessable", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

// Finished rounds are one per completed contribution, so they have to expire --
// but a settled outcome stays reportable while it is inside the retention.
func TestSweepDropsSettledSessionsAfterRetention(t *testing.T) {
	base := time.Now()
	donations := &Donations{
		sessions: map[string]*DonationSession{},
		clock:    func() time.Time { return base },
	}
	donations.sessions["don_old_settled"] = &DonationSession{
		ID: "don_old_settled", Status: "credited",
		CreatedAt: base.Add(-donationSettledRetention - time.Hour),
	}
	donations.sessions["don_recent_settled"] = &DonationSession{
		ID: "don_recent_settled", Status: "credited",
		CreatedAt: base.Add(-time.Minute),
	}
	donations.sessions["don_old_pending"] = &DonationSession{
		ID: "don_old_pending", Status: "pending",
		CreatedAt: base.Add(-donationSessionTTL - time.Hour),
	}

	donations.mu.Lock()
	donations.sweepLocked()
	donations.mu.Unlock()

	if _, ok := donations.sessions["don_old_settled"]; ok {
		t.Fatal("a settled session past its retention must be dropped")
	}
	if _, ok := donations.sessions["don_old_pending"]; ok {
		t.Fatal("an expired pending session must still be dropped")
	}
	if _, ok := donations.sessions["don_recent_settled"]; !ok {
		t.Fatal("a settled session inside the retention must stay reportable")
	}
}
