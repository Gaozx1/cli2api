package recovery

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

type fakeStore struct {
	mu        sync.Mutex
	accounts  []accounts.Account
	logs      []accounts.RequestLog
	attempts  []accounts.RequestAttempt
	insertErr error
}

func (f *fakeStore) List(context.Context) ([]accounts.Account, error) { return f.accounts, nil }

func (f *fakeStore) InsertRequestLog(_ context.Context, log accounts.RequestLog) error {
	if f.insertErr != nil {
		return f.insertErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = append(f.logs, log)
	return nil
}

func (f *fakeStore) InsertRequestAttempt(_ context.Context, attempt accounts.RequestAttempt) error {
	if f.insertErr != nil {
		return f.insertErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts = append(f.attempts, attempt)
	return nil
}

type fakeAccounts struct {
	mu       sync.Mutex
	updates  map[string]accounts.UpdateAccount
	failWith error
}

func (f *fakeAccounts) Update(_ context.Context, id string, input accounts.UpdateAccount) (accounts.Account, error) {
	if f.failWith != nil {
		return accounts.Account{}, f.failWith
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.updates == nil {
		f.updates = map[string]accounts.UpdateAccount{}
	}
	f.updates[id] = input
	return accounts.Account{ID: id, Enabled: true}, nil
}

// fakeAdapter serves a catalog and answers only the models it is told to.
type fakeAdapter struct {
	models     []providers.ModelInfo
	catalogErr error
	answers    map[string]bool // model -> succeeds
	calls      []string
}

func (f *fakeAdapter) Models(context.Context, string) ([]providers.ModelInfo, error) {
	if f.catalogErr != nil {
		return nil, f.catalogErr
	}
	return f.models, nil
}

func (f *fakeAdapter) ChatNonStream(_ context.Context, _ string, req translate.ChatRequest) (providers.ChatOutcome, error) {
	f.calls = append(f.calls, req.Model)
	if f.answers[req.Model] {
		return providers.ChatOutcome{Content: "ok"}, nil
	}
	return providers.ChatOutcome{}, &providers.Error{
		Kind: accounts.KindInvalidRequest, Status: 400, Code: "11140",
		Message: `{"code":11140,"msg":"request illegal"}`,
	}
}

func (f *fakeAdapter) ChatStream(context.Context, string, translate.ChatRequest) (*http.Response, providers.ResolvedChat, error) {
	return nil, providers.ResolvedChat{}, errors.New("unused in tests")
}

func registryWith(fake *fakeAdapter) *providers.Registry {
	reg := providers.NewRegistry()
	reg.Register(providers.Adapter{ID: "workbuddy", Models: fake, Chat: fake})
	return reg
}

func model(name, credits string) providers.ModelInfo {
	return providers.ModelInfo{NativeModel: name, PublicModel: name, Credits: credits}
}

func disabled(id string) accounts.Account {
	return accounts.Account{ID: id, Name: id, Provider: "workbuddy", Enabled: false}
}

// A disabled account whose cheapest model answers is turned back on.
func TestRecoversAccountWhenCheapestModelAnswers(t *testing.T) {
	store := &fakeStore{accounts: []accounts.Account{disabled("acc1")}}
	svc := &fakeAccounts{}
	fake := &fakeAdapter{
		models:  []providers.ModelInfo{model("pricey", "x6.67"), model("cheap", "x0.03")},
		answers: map[string]bool{"cheap": true},
	}
	n, err := New(store, svc, registryWith(fake)).Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("recovered %d, want 1", n)
	}
	got, ok := svc.updates["acc1"]
	if !ok || got.Enabled == nil || !*got.Enabled {
		t.Fatalf("acc1 was not re-enabled: %+v", got)
	}
	// The cheap model is tried first, so the expensive one is never spent on a
	// probe.
	if len(fake.calls) == 0 || fake.calls[0] != "cheap" {
		t.Fatalf("calls = %v, want the cheapest model first", fake.calls)
	}
}

// A still-broken account is left disabled.
func TestStillBrokenAccountStaysDisabled(t *testing.T) {
	store := &fakeStore{accounts: []accounts.Account{disabled("acc1")}}
	svc := &fakeAccounts{}
	fake := &fakeAdapter{
		models:  []providers.ModelInfo{model("cheap", "x0.03"), model("mid", "x0.59")},
		answers: map[string]bool{},
	}
	n, err := New(store, svc, registryWith(fake)).Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("recovered %d, want 0", n)
	}
	if _, ok := svc.updates["acc1"]; ok {
		t.Fatal("a still-broken account must not be re-enabled")
	}
}

// Enabled accounts are not probed at all.
func TestEnabledAccountsAreSkipped(t *testing.T) {
	live := disabled("acc1")
	live.Enabled = true
	store := &fakeStore{accounts: []accounts.Account{live}}
	fake := &fakeAdapter{models: []providers.ModelInfo{model("cheap", "x0.03")}, answers: map[string]bool{"cheap": true}}
	n, err := New(store, &fakeAccounts{}, registryWith(fake)).Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || len(fake.calls) != 0 {
		t.Fatalf("an enabled account must not be probed (n=%d calls=%v)", n, fake.calls)
	}
}

// Without a catalog there is no cheap model to pick, so nothing is probed.
func TestUnavailableCatalogProbesNothing(t *testing.T) {
	store := &fakeStore{accounts: []accounts.Account{disabled("acc1")}}
	svc := &fakeAccounts{}
	fake := &fakeAdapter{answers: map[string]bool{}, catalogErr: errors.New("catalog down")}
	n, _ := New(store, svc, registryWith(fake)).Sweep(context.Background())
	if n != 0 || len(fake.calls) != 0 {
		t.Fatalf("recovered %d calls=%v, want none", n, fake.calls)
	}
}

// At most ModelsPerAccount models are tried per account per round.
func TestModelAttemptsAreCapped(t *testing.T) {
	store := &fakeStore{accounts: []accounts.Account{disabled("acc1")}}
	fake := &fakeAdapter{
		models: []providers.ModelInfo{
			model("m1", "x0.01"), model("m2", "x0.02"), model("m3", "x0.03"), model("m4", "x0.04"),
		},
		answers: map[string]bool{}, // none work, so every candidate is tried
	}
	if _, err := New(store, &fakeAccounts{}, registryWith(fake)).Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fake.calls) != ModelsPerAccount {
		t.Fatalf("tried %d models, want the cap %d", len(fake.calls), ModelsPerAccount)
	}
}

// Every probe is recorded as a normal attempt, so the content-review rule sees
// it and cannot re-disable an account on a stale failure streak.
func TestProbeIsRecordedAsAttempt(t *testing.T) {
	store := &fakeStore{accounts: []accounts.Account{disabled("acc1")}}
	fake := &fakeAdapter{models: []providers.ModelInfo{model("cheap", "x0.03")}, answers: map[string]bool{"cheap": true}}
	if _, err := New(store, &fakeAccounts{}, registryWith(fake)).Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.logs) != 1 || len(store.attempts) != 1 {
		t.Fatalf("logs=%d attempts=%d, want 1 each", len(store.logs), len(store.attempts))
	}
	if store.logs[0].Status != "ok" || store.attempts[0].Status != accounts.AttemptStatusOK {
		t.Fatalf("a successful probe must be recorded as ok: %+v / %+v", store.logs[0], store.attempts[0])
	}
	if store.attempts[0].AccountID != "acc1" {
		t.Fatalf("attempt account = %q", store.attempts[0].AccountID)
	}
}

// A failing probe is recorded with the provider's error, so the history is
// truthful.
func TestFailedProbeRecordsError(t *testing.T) {
	store := &fakeStore{accounts: []accounts.Account{disabled("acc1")}}
	fake := &fakeAdapter{models: []providers.ModelInfo{model("cheap", "x0.03")}, answers: map[string]bool{}}
	if _, err := New(store, &fakeAccounts{}, registryWith(fake)).Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(store.attempts))
	}
	if store.attempts[0].Status != accounts.AttemptStatusError {
		t.Fatalf("status = %q, want error", store.attempts[0].Status)
	}
	if store.attempts[0].ErrorKind != accounts.KindInvalidRequest {
		t.Fatalf("kind = %q, want %q", store.attempts[0].ErrorKind, accounts.KindInvalidRequest)
	}
}

// A re-enable that fails must not be counted.
func TestFailedReEnableIsNotCounted(t *testing.T) {
	store := &fakeStore{accounts: []accounts.Account{disabled("acc1")}}
	svc := &fakeAccounts{failWith: errors.New("runtime down")}
	fake := &fakeAdapter{models: []providers.ModelInfo{model("cheap", "x0.03")}, answers: map[string]bool{"cheap": true}}
	n, err := New(store, svc, registryWith(fake)).Sweep(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("recovered %d, want 0 when the re-enable failed", n)
	}
}

// creditsCost orders the provider's cost text, treating unknown as expensive.
func TestCreditsCost(t *testing.T) {
	cases := []struct {
		credits string
		free    bool
		want    float64
		unknown bool
	}{
		{credits: "x0.03", want: 0.03},
		{credits: "x6.67 credits", want: 6.67},
		{credits: "x0.59", want: 0.59},
		{credits: "free", want: 0},
		{credits: "", free: true, want: 0},
		{credits: "", unknown: true},
		{credits: "unparseable", unknown: true},
	}
	for _, c := range cases {
		got := creditsCost(c.credits, c.free)
		if c.unknown {
			if got <= 1e307 {
				t.Fatalf("creditsCost(%q)=%v, want a very large sentinel", c.credits, got)
			}
			continue
		}
		if got != c.want {
			t.Fatalf("creditsCost(%q)=%v, want %v", c.credits, got, c.want)
		}
	}
}

// The interval is the requested 30 minutes.
func TestIntervalIsThirtyMinutes(t *testing.T) {
	if Interval != 30*time.Minute {
		t.Fatalf("Interval = %v, want 30m", Interval)
	}
}
