// Package recovery periodically re-tests disabled accounts and turns back on
// the ones that work again.
//
// An account leaves rotation for reasons that are often not permanent: a
// provider ban can be lifted, a quota can refill, a credential can be
// re-authorized. Nothing in the gateway noticed, so a recovered account stayed
// off until an operator happened to look at it. This loop sends each disabled
// account a minimal call on its cheapest model and re-enables it when that call
// succeeds.
//
// Two details matter for correctness:
//
//   - The cheapest model is chosen deliberately. A test call spends the
//     operator's credits, so it must not run on an expensive model. The
//     provider's own cost multiplier picks it, and the count of models tried
//     per account is capped.
//   - Every probe is recorded as a real request attempt. The content-review rule
//     decides from the account's recent call history, so a probe that is not
//     recorded would let a stale failure streak re-disable an account that has
//     just proven it works.
package recovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

const (
	// Interval is how often disabled accounts are re-tested.
	Interval = 30 * time.Minute

	// CallTimeout bounds a single test call.
	CallTimeout = 45 * time.Second

	// ModelsPerAccount caps how many models are tried before an account is left
	// for the next round. An account is recovered as soon as one model answers,
	// and the cap keeps a disabled account with a wide catalog from becoming a
	// request storm every 30 minutes.
	ModelsPerAccount = 2

	// probePrompt is intentionally trivial: the point is to prove the account
	// can serve, not to exercise the model.
	probePrompt = "ping"
)

// Store is the slice of the account store this loop needs.
type Store interface {
	List(ctx context.Context) ([]accounts.Account, error)
	InsertRequestLog(ctx context.Context, log accounts.RequestLog) error
	InsertRequestAttempt(ctx context.Context, attempt accounts.RequestAttempt) error
}

// AccountService applies the state change through the runtime, so a recovered
// account is started (or its worker resumed) rather than merely flagged on.
type AccountService interface {
	Update(ctx context.Context, id string, input accounts.UpdateAccount) (accounts.Account, error)
}

// Monitor re-tests disabled accounts.
type Monitor struct {
	store     Store
	accounts  AccountService
	providers *providers.Registry
	interval  time.Duration
}

// New builds a monitor.
func New(store Store, accountService AccountService, registry *providers.Registry) *Monitor {
	return &Monitor{store: store, accounts: accountService, providers: registry, interval: Interval}
}

// RunLoop sweeps every interval until stop closes. It does not sweep at
// startup: that would spend a request per disabled account on every boot, and
// the first interval is soon enough for accounts that have been off for a while.
func (m *Monitor) RunLoop(stop <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n, err := m.Sweep(ctx); err != nil {
				log.Printf("recovery sweep: %v", err)
			} else if n > 0 {
				log.Printf("recovery sweep: re-enabled %d recovered account(s)", n)
			}
		}
	}
}

// Sweep tests every disabled account once and re-enables the ones that answer.
// It returns how many were recovered.
func (m *Monitor) Sweep(ctx context.Context) (int, error) {
	all, err := m.store.List(ctx)
	if err != nil {
		return 0, fmt.Errorf("list accounts: %w", err)
	}
	recovered := 0
	for _, account := range all {
		if account.Enabled {
			continue
		}
		if err := ctx.Err(); err != nil {
			return recovered, err
		}
		if !m.accountAnswers(ctx, account) {
			continue
		}
		on := true
		clear := ""
		if _, err := m.accounts.Update(ctx, account.ID, accounts.UpdateAccount{
			Enabled: &on, LastError: &clear, LastErrorKind: &clear,
		}); err != nil {
			log.Printf("recovery: re-enable account=%s failed: %v", account.ID, err)
			continue
		}
		log.Printf("recovery: re-enabled account=%s name=%q provider=%s - a test call succeeded",
			account.ID, account.Name, account.Provider)
		recovered++
	}
	return recovered, nil
}

// accountAnswers reports whether any of the account's cheapest models answers a
// minimal call.
func (m *Monitor) accountAnswers(ctx context.Context, account accounts.Account) bool {
	adapter, ok := m.adapterFor(account)
	if !ok || adapter.Chat == nil || adapter.Models == nil {
		// Without a catalog we cannot pick a cheap model, and without chat we
		// cannot test at all. Leave it alone rather than guess.
		return false
	}
	models := cheapestModels(ctx, adapter, account.ID, ModelsPerAccount)
	for _, model := range models {
		if m.modelAnswers(ctx, account, adapter, model) {
			return true
		}
		if err := ctx.Err(); err != nil {
			return false
		}
	}
	return false
}

// adapterFor resolves the account's adapter by provider family.
func (m *Monitor) adapterFor(account accounts.Account) (providers.Adapter, bool) {
	if m.providers == nil {
		return providers.Adapter{}, false
	}
	return m.providers.Get(accounts.NormalizeProviderFamily(account.Provider))
}

// modelAnswers sends one minimal non-streaming call and reports whether the
// account served it.
func (m *Monitor) modelAnswers(ctx context.Context, account accounts.Account, adapter providers.Adapter, model string) bool {
	callCtx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()

	request := translate.ChatRequest{
		Model:     model,
		Messages:  []translate.ChatMessage{{Role: "user", Content: probePrompt}},
		MaxTokens: json.RawMessage("1"),
	}

	started := time.Now().UTC()
	outcome, err := adapter.Chat.ChatNonStream(callCtx, account.ID, request)
	finished := time.Now().UTC()

	m.recordProbe(ctx, account, model, started, finished, outcome, err)

	if err != nil {
		if isTransient(err) {
			log.Printf("recovery: account=%s model=%s could not be reached (transient): %v", account.ID, model, err)
		}
		return false
	}
	return true
}

// recordProbe writes the probe as a normal request log plus one attempt, so the
// account's recent history reflects it.
func (m *Monitor) recordProbe(ctx context.Context, account accounts.Account, model string, started, finished time.Time, outcome providers.ChatOutcome, callErr error) {
	requestID := accounts.NewRequestID()
	attemptID := accounts.NewAttemptID()
	latency := int(finished.Sub(started).Milliseconds())

	logEntry := accounts.RequestLog{
		ID:             requestID,
		CreatedAt:      started,
		FinishedAt:     &finished,
		Status:         "error",
		RequestedModel: model,
		AccountID:      account.ID,
		Provider:       account.Provider,
		Routing:        "probe",
		LatencyMs:      &latency,
		AttemptCount:   1,
		MessageCount:   1,
		MessageRoles:   []string{"user"},
	}
	attempt := accounts.RequestAttempt{
		ID:           attemptID,
		RequestID:    requestID,
		AttemptIndex: 0,
		AccountID:    account.ID,
		StartedAt:    started,
		FinishedAt:   &finished,
		Status:       accounts.AttemptStatusError,
		LatencyMs:    &latency,
	}

	if callErr == nil {
		logEntry.Status = "ok"
		logEntry.ErrorKind = ""
		attempt.Status = accounts.AttemptStatusOK
		if outcome.UsageSource != "" {
			logEntry.UsageSource = outcome.UsageSource
		}
	} else {
		kind, code, message := describe(callErr)
		logEntry.ErrorKind = kind
		logEntry.ErrorCode = code
		logEntry.ErrorMessage = message
		attempt.ErrorKind = kind
		attempt.ErrorMessage = message
	}

	if err := m.store.InsertRequestLog(ctx, logEntry); err != nil {
		log.Printf("recovery: record probe log for %s: %v", account.ID, err)
		return
	}
	if err := m.store.InsertRequestAttempt(ctx, attempt); err != nil {
		log.Printf("recovery: record probe attempt for %s: %v", account.ID, err)
	}
}

// describe pulls the kind, code and message out of a provider error so the
// recorded probe looks like any other attempt.
func describe(err error) (string, string, string) {
	var classified *providers.Error
	if errors.As(err, &classified) {
		return classified.Kind, classified.Code, classified.Message
	}
	return accounts.KindUnavailable, "", err.Error()
}

// isTransient reports whether an error says nothing about the credential: a
// transport failure or an upstream 5xx is an outage, not a verdict.
func isTransient(err error) bool {
	var classified *providers.Error
	if errors.As(err, &classified) {
		return classified.Kind == accounts.KindUnavailable
	}
	return true
}

// cheapestModels returns up to limit model ids ordered by the provider's own
// cost multiplier, cheapest first.
func cheapestModels(ctx context.Context, adapter providers.Adapter, accountID string, limit int) []string {
	if limit <= 0 {
		return nil
	}
	models, err := adapter.Models.Models(ctx, accountID)
	if err != nil {
		return nil
	}
	type candidate struct {
		name string
		cost float64
	}
	candidates := make([]candidate, 0, len(models))
	for _, info := range models {
		name := strings.TrimSpace(info.NativeModel)
		if name == "" {
			name = strings.TrimSpace(info.PublicModel)
		}
		if name == "" {
			continue
		}
		candidates = append(candidates, candidate{name: name, cost: creditsCost(info.Credits, info.Free)})
	}
	// Stable so models of equal cost keep the provider's order.
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].cost < candidates[j].cost })

	out := make([]string, 0, limit)
	for _, c := range candidates {
		if len(out) == limit {
			break
		}
		out = append(out, c.name)
	}
	return out
}

// creditsCost turns the provider's cost text ("x0.03 credits", "free") into a
// number so the cheapest model can be picked. An unlabelled or unparseable cost
// sorts last: an unknown model is not assumed to be cheap.
func creditsCost(credits string, free bool) float64 {
	if free {
		return 0
	}
	text := strings.ToLower(strings.TrimSpace(credits))
	if text == "" {
		return math.MaxFloat64
	}
	if strings.Contains(text, "free") {
		return 0
	}
	start, end := -1, -1
	for i, r := range text {
		if r == '.' || (r >= '0' && r <= '9') {
			if start < 0 {
				start = i
			}
			end = i + 1
			continue
		}
		if start >= 0 {
			break
		}
	}
	if start < 0 || end <= start {
		return math.MaxFloat64
	}
	value, err := strconv.ParseFloat(text[start:end], 64)
	if err != nil {
		return math.MaxFloat64
	}
	return value
}
