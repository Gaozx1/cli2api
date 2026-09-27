package control

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
)

// Donations accepts a contributed provider account and credits the contributor
// on a New API site in exchange.
//
// The flow is deliberately ordered so a contributor is never credited for an
// account the pool rejected: import first, then credit. A credit failure after
// a successful import leaves the account in the pool and is reported to the
// caller rather than rolled back, because an imported account is only ever
// additive and the contributor can be credited again by an operator.
type Donations struct {
	settings *Settings
	accounts *Accounts
	// HTTP is injectable so tests never reach a live site.
	HTTP *http.Client
	// BaseURL and Token default from Settings when unset, so the console can
	// configure the target site without a restart.
	BaseURL string
	Token   string

	// mu guards sessions. A contribution only stays pending while the
	// contributor is authorizing in the browser, so the map is process-local
	// and rebuilt on restart; a swept session leaves no reward behind.
	mu       sync.Mutex
	sessions map[string]*DonationSession
	// clock is injectable for tests.
	clock func() time.Time
}

// now is injectable so tests can drive session expiry without sleeping.
func (d *Donations) now() time.Time {
	if d != nil && d.clock != nil {
		return d.clock()
	}
	return time.Now()
}

// Donation secrets. The site is configured in the console like other system
// settings; the token is a New API user AccessToken (个人设置 - 系统访问令牌).
const (
	donationBaseURLSecret = "donation_newapi_base_url"
	donationTokenSecret   = "donation_newapi_token"
	// donationQuotaPerUSD is the New API quota unit count for one USD. New API
	// stores quota as an integer and renders it as quota / QuotaPerUnit dollars.
	donationQuotaPerUSD = 500000
	// donationQoderFormat is the native Qoder credential format, which carries a
	// raw auth blob rather than a JSON credential like the other providers.
	donationQoderFormat = "qoder-native-v1"
)

// DonationSessionTTL bounds how long a started web-authorization round may wait
// before the pending account is swept. The provider login must finish inside it.
const donationSessionTTL = 15 * time.Minute

// donationDefaultUSD is the reward when the caller does not specify one.
const donationDefaultUSD = 1.0

// donationMaxUSD caps a single reward. The endpoints that reach it are public
// and unauthenticated, and the reward is paid out of the operator's own New API
// quota, so the caller must never be able to name an arbitrary amount.
const donationMaxUSD = 50.0

// donationSettledRetention bounds how long a finished session stays in memory so
// its outcome can still be reported. Pending sessions use donationSessionTTL.
// Without this, a settled session -- one per completed contribution -- would
// live forever, since the sweep only ever looked at pending ones.
const donationSettledRetention = 24 * time.Hour

// donationSessionID returns an opaque id for one authorization round.
//
// It is drawn from crypto/rand rather than composed from the clock: a session id
// is the only thing standing between an anonymous caller and somebody else's
// in-flight round, because GET reports the authorization URL (OAuth state
// included) and DELETE abandons the round.
func donationSessionID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand does not fail in practice; degrade to a clock-derived id
		// rather than handing back a constant.
		return "don_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return "don_" + hex.EncodeToString(buf[:])
}

// DonationSession is one in-flight web authorization: the contributed account
// exists and is being authorized in the browser, but the reward is not credited
// until the provider login completes.
type DonationSession struct {
	ID            string    `json:"session_id"`
	AccountID     string    `json:"account_id"`
	Provider      string    `json:"provider"`
	Region        string    `json:"region"`
	Name          string    `json:"name"`
	Format        string    `json:"format"`
	NewAPIUserID  int       `json:"newapi_user_id"`
	CreditUSD     float64   `json:"credit_usd"`
	AuthURL       string    `json:"auth_url"`
	Status        string    `json:"status"`
	Message       string    `json:"message,omitempty"`
	Credited      bool      `json:"credited"`
	CreditedQuota int       `json:"credited_quota,omitempty"`
	CreditError   string    `json:"credit_error,omitempty"`
	CreatedAt     time.Time `json:"created_at"`

	// settling marks a round whose settle path has been claimed. It is not part
	// of the wire contract; it exists so two concurrent polls cannot both pay
	// out. Guarded by Donations.mu.
	settling bool
}

// donationSettled reports whether a round has reached a terminal state.
func donationSettled(record *DonationSession) bool {
	return record.Status == "credited" || record.Status == "failed"
}

// DonationStart is the input for beginning a web-authorized contribution.
type DonationStart struct {
	Format       string  `json:"format"`
	Name         string  `json:"name"`
	Region       string  `json:"region"`
	NewAPIUserID int     `json:"newapi_user_id"`
	CreditUSD    float64 `json:"credit_usd"`
}

// DonationCredential is the input for a pasted-credential contribution.
//
// Deprecated: superseded by the web-authorization flow, which never handles a
// contributor's raw credential. Kept so the existing endpoint keeps working.
type DonationRequest struct {
	// Format is the credential format being contributed
	// ("qoder-native-v1" or "workbuddy-oauth-v1").
	Format string `json:"format"`
	// Name labels the account in the console.
	Name string `json:"name"`
	// Region pins the provider region ("global" or "cn").
	Region string `json:"region"`
	// NewAPIUserID is the contributor's numeric New API user id.
	NewAPIUserID int `json:"newapi_user_id"`
	// CreditUSD is the reward in USD. Zero means the server default.
	CreditUSD float64 `json:"credit_usd"`
	// Credential carries the provider payload. For WorkBuddy this is the
	// canonical flat credential; for qoder the raw user blob and machine id.
	Credential json.RawMessage `json:"credential"`
	// UserBlob is base64 qoder auth; only read for qoder-native-v1.
	UserBlob  string `json:"user_blob"`
	MachineID string `json:"machine_id"`
}

// DonationResult reports what happened, including the credited amount so the
// page can show a receipt.
type DonationResult struct {
	AccountID     string  `json:"account_id"`
	AccountName   string  `json:"account_name"`
	Provider      string  `json:"provider"`
	Region        string  `json:"region"`
	Status        string  `json:"status"`
	CreditedUSD   float64 `json:"credited_usd"`
	CreditedQuota int     `json:"credited_quota"`
	Credited      bool    `json:"credited"`
	CreditError   string  `json:"credit_error,omitempty"`
}

func NewDonations(settings *Settings, accountService *Accounts) *Donations {
	return &Donations{settings: settings, accounts: accountService}
}

func (d *Donations) httpClient() *http.Client {
	if d != nil && d.HTTP != nil {
		return d.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// siteConfig resolves the target site and token, preferring the explicit field
// and falling back to persisted settings.
func (d *Donations) siteConfig(ctx context.Context) (base, token string) {
	base, token = d.BaseURL, d.Token
	if d.settings != nil {
		if base == "" {
			base, _, _ = d.settings.GetSecret(ctx, donationBaseURLSecret)
		}
		if token == "" {
			token, _, _ = d.settings.GetSecret(ctx, donationTokenSecret)
		}
	}
	return strings.TrimRight(strings.TrimSpace(base), "/"), strings.TrimSpace(token)
}

// QuotaForUSD converts a USD reward into New API quota units.
func QuotaForUSD(usd float64) int {
	if usd <= 0 {
		return 0
	}
	return int(usd*donationQuotaPerUSD + 0.5)
}

// normalizeDonationUSD resolves the requested reward: the default when the
// caller omitted it, an error above donationMaxUSD.
//
// Both donation entry points are public, and this value is what gets added to a
// New API account straight from the operator's own quota, so the cap is applied
// before the account is created rather than at the edge.
func normalizeDonationUSD(usd float64) (float64, error) {
	if usd <= 0 {
		return donationDefaultUSD, nil
	}
	if usd > donationMaxUSD {
		return 0, operationError(
			"invalid_credit_usd",
			fmt.Sprintf("credit_usd must not exceed %.0f", donationMaxUSD),
		)
	}
	return usd, nil
}

// creditQuota adds quota to one New API user.
//
// New API validates /api/user/manage with the user AccessToken in the
// Authorization header plus the matching numeric id in New-Api-User; omitting
// the latter fails with auth.user_id_not_provided.
func (d *Donations) creditQuota(ctx context.Context, userID, quota int) error {
	base, token := d.siteConfig(ctx)
	if base == "" || token == "" {
		return fmt.Errorf("donation site is not configured")
	}
	body, err := json.Marshal(map[string]any{
		"id":     userID,
		"action": "add_quota",
		"mode":   "add",
		"value":  quota,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/user/manage", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("New-Api-User", strconv.Itoa(userID))

	resp, err := d.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("donation site unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))

	var parsed struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(raw, &parsed)
	if resp.StatusCode != http.StatusOK {
		if parsed.Message != "" {
			return fmt.Errorf("donation site rejected the credit: %s", parsed.Message)
		}
		return fmt.Errorf("donation site returned HTTP %d", resp.StatusCode)
	}
	if !parsed.Success {
		if parsed.Message != "" {
			return fmt.Errorf("donation site rejected the credit: %s", parsed.Message)
		}
		return fmt.Errorf("donation site rejected the credit")
	}
	return nil
}

// Submit imports the contributed account and credits the contributor.
//
// Deprecated: prefer StartSession, which authorizes the account through the
// provider's own browser login instead of accepting a pasted credential.
func (d *Donations) Submit(ctx context.Context, input DonationRequest) (DonationResult, error) {
	providerID, err := donationProvider(input.Format)
	if err != nil {
		return DonationResult{}, err
	}
	if input.NewAPIUserID <= 0 {
		return DonationResult{}, operationError("invalid_newapi_user_id", "a numeric New API user id is required")
	}
	region := strings.ToLower(strings.TrimSpace(input.Region))
	if region == "" {
		region = defaultDonationRegion(providerID)
	}

	// Validate the reward before importing: a rejected reward must not leave a
	// contributed account behind.
	usd, err := normalizeDonationUSD(input.CreditUSD)
	if err != nil {
		return DonationResult{}, err
	}

	created, err := d.importAccount(ctx, providerID, region, input)
	if err != nil {
		return DonationResult{}, err
	}

	quota := QuotaForUSD(usd)

	result := DonationResult{
		AccountID:     created.ID,
		AccountName:   created.Name,
		Provider:      providerID,
		Region:        region,
		Status:        created.Status,
		CreditedUSD:   usd,
		CreditedQuota: quota,
	}
	// A credit failure must not discard a valid contributed account; surface it
	// so the operator can credit manually.
	if err := d.creditQuota(ctx, input.NewAPIUserID, quota); err != nil {
		result.CreditError = err.Error()
		return result, nil
	}
	result.Credited = true
	return result, nil
}

// donationStrippedCredentialFields are credential fields that select the host a
// provider talks to. Providers accept them so an operator can point an account
// at a non-default endpoint (command/client.go, devin/client.go,
// trae/client.go all prefer the credential value over their constant).
//
// A contributed credential is untrusted input, and once the account is enabled
// the pool routes other people's requests through it, so a contributor must not
// be able to name that host.
var donationStrippedCredentialFields = map[string]struct{}{
	"base_url": {},
	"baseUrl":  {},
	"api_host": {},
	"apiHost":  {},
}

// stripDonationCredentialHosts removes upstream-host overrides from a
// contributed credential. Providers read them at the top level and, for Trae,
// nested under "auth", so the walk is recursive. A payload that is not a JSON
// object is returned untouched and left to the provider importer to reject.
func stripDonationCredentialHosts(payload []byte) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(payload, &doc); err != nil || doc == nil {
		return payload, nil
	}
	stripDonationHostFields(doc)
	encoded, err := json.Marshal(doc)
	if err != nil {
		return payload, nil
	}
	return encoded, nil
}

func stripDonationHostFields(node any) {
	switch value := node.(type) {
	case map[string]any:
		for key, child := range value {
			if _, drop := donationStrippedCredentialFields[key]; drop {
				delete(value, key)
				continue
			}
			stripDonationHostFields(child)
		}
	case []any:
		for _, child := range value {
			stripDonationHostFields(child)
		}
	}
}

func (d *Donations) importAccount(ctx context.Context, providerID, region string, input DonationRequest) (accounts.Account, error) {
	if d.accounts == nil {
		return accounts.Account{}, operationError("donations_unavailable", "account service is unavailable")
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		name = providerID + " donation"
	}

	// The account is created disabled, in both branches. Nothing about a
	// contributed credential has been checked against the provider, and enabled
	// accounts carry other people's traffic, so enabling is an operator action.
	if input.Format == donationQoderFormat {
		return d.accounts.Import(ctx, AccountImportInput{
			Format:    input.Format,
			Name:      name,
			Provider:  providerID,
			Region:    region,
			Enabled:   false,
			UserBlob:  strings.TrimSpace(input.UserBlob),
			MachineID: strings.TrimSpace(input.MachineID),
		}, nil)
	}

	payload := input.Credential
	if len(payload) == 0 {
		return accounts.Account{}, operationError("invalid_credential", "credential is required")
	}
	sanitized, err := stripDonationCredentialHosts(payload)
	if err != nil {
		return accounts.Account{}, operationError("invalid_credential", err.Error())
	}
	return d.accounts.Import(ctx, AccountImportInput{
		Format:     input.Format,
		Name:       name,
		Provider:   providerID,
		Region:     region,
		Enabled:    false,
		Credential: sanitized,
	}, sanitized)
}

// donationProvider maps a contribution format to its provider family.
func donationProvider(format string) (string, error) {
	for _, descriptor := range providers.List() {
		if descriptor.SupportsCredentialFormat(format) {
			return descriptor.ID, nil
		}
	}
	return "", operationError("unsupported_format", "unsupported donation format")
}

func defaultDonationRegion(providerID string) string {
	if descriptor, ok := providers.Get(providerID); ok {
		if descriptor.DefaultRegion != "" {
			return descriptor.DefaultRegion
		}
	}
	return "global"
}

// donationSupportsWebAuth reports whether a provider can authorize a
// contribution through its own browser login. Qoder's child runtime exposes
// device login through its worker, and every in-process provider that ships a
// LoginSessionProvider can do the same; providers without one must fall back to
// a pasted credential.
func (d *Donations) donationSupportsWebAuth(providerID string) bool {
	if d == nil || d.accounts == nil || d.accounts.Providers == nil {
		return false
	}
	adapter, ok := d.accounts.Providers.Get(providerID)
	return ok && adapter.Login != nil
}

func (d *Donations) sessionStore() map[string]*DonationSession {
	if d.sessions == nil {
		d.sessions = map[string]*DonationSession{}
	}
	return d.sessions
}

// StartSession opens a web-authorization round for a contributed account.
//
// It creates the account disabled, begins the provider's browser login, and
// records the pending contribution. The reward is credited only when the
// authorization completes (see PollSession), so a half-finished round never
// pays out. Nothing is credited here.
func (d *Donations) StartSession(ctx context.Context, input DonationStart) (DonationSession, error) {
	providerID, err := donationProvider(input.Format)
	if err != nil {
		return DonationSession{}, err
	}
	if input.NewAPIUserID <= 0 {
		return DonationSession{}, operationError("invalid_newapi_user_id", "a numeric New API user id is required")
	}
	if d.accounts == nil {
		return DonationSession{}, operationError("donations_unavailable", "account service is unavailable")
	}
	if !d.donationSupportsWebAuth(providerID) {
		return DonationSession{}, operationError("web_auth_unsupported", "this provider does not support web authorization")
	}
	region := strings.ToLower(strings.TrimSpace(input.Region))
	if region == "" {
		region = defaultDonationRegion(providerID)
	}
	usd, err := normalizeDonationUSD(input.CreditUSD)
	if err != nil {
		return DonationSession{}, err
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		name = providerID + " donation"
	}

	// The account starts disabled: it must not carry traffic before the
	// contributor has actually authorized it.
	created, err := d.accounts.Create(ctx, accounts.CreateAccount{
		Name: name, Provider: providerID, Region: region, Enabled: false,
	})
	if err != nil {
		return DonationSession{}, operationError("account_create_failed", err.Error())
	}

	session, err := d.accounts.StartLogin(ctx, created.ID)
	if err != nil {
		// Nothing was authorized, so drop the placeholder account rather than
		// leaving an unusable entry in the pool.
		_ = d.accounts.Delete(ctx, created.ID)
		return DonationSession{}, err
	}

	record := &DonationSession{
		ID:           donationSessionID(),
		AccountID:    created.ID,
		Provider:     providerID,
		Region:       region,
		Name:         name,
		Format:       input.Format,
		NewAPIUserID: input.NewAPIUserID,
		CreditUSD:    usd,
		AuthURL:      session.AuthURL,
		Status:       "pending",
		Message:      "open the authorization URL to finish login",
		CreatedAt:    d.now(),
	}
	d.mu.Lock()
	expired := d.sweepLocked()
	d.sessionStore()[record.ID] = record
	d.mu.Unlock()
	d.deleteSweptAccounts(ctx, expired)
	return *record, nil
}

// PollSession reports progress on a web-authorized contribution. Once the
// provider login reports done it enables the account and credits the
// contributor exactly once.
func (d *Donations) PollSession(ctx context.Context, sessionID string) (DonationSession, error) {
	d.mu.Lock()
	expired := d.sweepLocked()
	record, ok := d.sessionStore()[sessionID]
	// Snapshot under the lock, then act outside it. A settled or
	// already-claimed round reports what it knows; only the poll that wins the
	// claim below is allowed to credit.
	var early DonationSession
	claimed := false
	if ok {
		early = *record
		claimed = !record.settling && !donationSettled(record)
	}
	d.mu.Unlock()
	d.deleteSweptAccounts(ctx, expired)

	if !ok {
		return DonationSession{}, operationError("not_found", "unknown donation session")
	}
	if !claimed {
		return early, nil
	}

	done, message, err := d.accounts.PollLogin(ctx, record.AccountID)
	if err != nil {
		return d.sessionSnapshot(sessionID), operationError("login_poll_failed", err.Error())
	}

	// Claim the settle path under the lock. PollLogin runs outside it because it
	// does I/O; the claim is what makes the credit happen exactly once when
	// several polls observe the same completed login.
	d.mu.Lock()
	if record.settling || donationSettled(record) {
		snapshot := *record
		d.mu.Unlock()
		return snapshot, nil
	}
	record.Message = message
	if !done {
		snapshot := *record
		d.mu.Unlock()
		return snapshot, nil
	}
	record.settling = true
	d.mu.Unlock()

	d.settle(ctx, record)
	return d.sessionSnapshot(sessionID), nil
}

// sessionSnapshot copies a session under the lock. Callers outside the critical
// section must never read session fields directly: settle writes them, and a
// torn struct copy is not something a caller can defend against.
func (d *Donations) sessionSnapshot(sessionID string) DonationSession {
	d.mu.Lock()
	defer d.mu.Unlock()
	record, ok := d.sessionStore()[sessionID]
	if !ok {
		return DonationSession{}
	}
	return *record
}

// CancelSession abandons a pending contribution and removes its placeholder
// account. A settled session is left alone so a finished reward is not lost.
func (d *Donations) CancelSession(ctx context.Context, sessionID string) error {
	d.mu.Lock()
	record, ok := d.sessionStore()[sessionID]
	if ok && (donationSettled(record) || record.settling) {
		d.mu.Unlock()
		return operationError("invalid_request", "this contribution has already finished")
	}
	delete(d.sessionStore(), sessionID)
	d.mu.Unlock()
	if !ok {
		return operationError("not_found", "unknown donation session")
	}
	if err := d.accounts.Delete(ctx, record.AccountID); err != nil {
		return operationError("account_delete_failed", err.Error())
	}
	return nil
}

// settle enables the authorized account and issues the reward once.
//
// It runs outside d.mu because both steps do I/O, and it never writes the
// session directly: the outcome is committed in a single locked update. That is
// what keeps a second poll from crediting again -- the caller claimed the round
// by setting settling before calling this.
func (d *Donations) settle(ctx context.Context, record *DonationSession) {
	accountID := record.AccountID
	userID := record.NewAPIUserID
	quota := QuotaForUSD(record.CreditUSD)

	// Enable+boot so the authorized credential is actually in the pool.
	enabled := true
	if _, err := d.accounts.Update(ctx, accountID, accounts.UpdateAccount{Enabled: &enabled}); err != nil {
		d.finishSettle(record.ID, "failed", 0, "",
			"authorization succeeded but the account could not be enabled: "+err.Error())
		return
	}

	if err := d.creditQuota(ctx, userID, quota); err != nil {
		// The account is valid and stays in the pool; only the reward is
		// outstanding, so an operator can credit it without re-authorizing.
		d.finishSettle(record.ID, "failed", quota, err.Error(),
			"account authorized and added, but the credit failed")
		return
	}
	d.finishSettle(record.ID, "credited", quota, "", "authorized and credited")
}

// finishSettle commits one settle outcome and releases the claim.
func (d *Donations) finishSettle(sessionID, status string, quota int, creditErr, message string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	record, ok := d.sessionStore()[sessionID]
	if !ok {
		return
	}
	record.Status = status
	record.CreditedQuota = quota
	record.CreditError = creditErr
	record.Message = message
	record.Credited = status == "credited"
	record.settling = false
}

// sweepLocked drops expired sessions and returns the placeholder accounts that
// have to go with them. Must be called with d.mu held.
//
// Deletion is returned rather than performed here: it reaches the account store
// (and the runtime), which must not happen while this lock is held, and doing it
// on a detached goroutine makes the caller's view of the store race with the
// sweep for no benefit -- the caller is already the only one who cares.
//
// Settled rounds expire too: there is one per completed contribution, and the
// endpoint that creates them is unauthenticated, so leaving them in the map
// forever is an unbounded memory growth path.
func (d *Donations) sweepLocked() []string {
	if d.sessions == nil {
		return nil
	}
	now := d.now()
	pendingCutoff := now.Add(-donationSessionTTL)
	settledCutoff := now.Add(-donationSettledRetention)
	var expired []string
	for id, record := range d.sessions {
		// A claimed round is mid-flight; whoever claimed it owns the entry.
		if record.settling {
			continue
		}
		if donationSettled(record) {
			if record.CreatedAt.After(settledCutoff) {
				continue
			}
			// The account is the contribution being paid for, so it stays.
			delete(d.sessions, id)
			continue
		}
		if record.CreatedAt.After(pendingCutoff) {
			continue
		}
		delete(d.sessions, id)
		expired = append(expired, record.AccountID)
	}
	return expired
}

// deleteSweptAccounts removes the placeholder accounts of expired rounds.
// Failures are dropped: the round is gone and an unenabled placeholder is inert.
func (d *Donations) deleteSweptAccounts(ctx context.Context, accountIDs []string) {
	if d == nil || d.accounts == nil || len(accountIDs) == 0 {
		return
	}
	for _, accountID := range accountIDs {
		_ = d.accounts.Delete(ctx, accountID)
	}
}
