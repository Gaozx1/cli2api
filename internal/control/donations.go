package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

// donationSessionID returns an opaque id for one authorization round. It only
// has to be unique within this process, since sessions are process-local.
func donationSessionID() string {
	return "don_" + strconv.FormatInt(time.Now().UnixNano(), 36) + strconv.FormatUint(nonce.Add(1), 36)
}

var nonce atomic.Uint64

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

	created, err := d.importAccount(ctx, providerID, region, input)
	if err != nil {
		return DonationResult{}, err
	}

	usd := input.CreditUSD
	if usd <= 0 {
		usd = 1
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

func (d *Donations) importAccount(ctx context.Context, providerID, region string, input DonationRequest) (accounts.Account, error) {
	if d.accounts == nil {
		return accounts.Account{}, operationError("donations_unavailable", "account service is unavailable")
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		name = providerID + " donation"
	}

	if input.Format == donationQoderFormat {
		return d.accounts.Import(ctx, AccountImportInput{
			Format:    input.Format,
			Name:      name,
			Provider:  providerID,
			Region:    region,
			Enabled:   true,
			UserBlob:  strings.TrimSpace(input.UserBlob),
			MachineID: strings.TrimSpace(input.MachineID),
		}, nil)
	}

	payload := input.Credential
	if len(payload) == 0 {
		return accounts.Account{}, operationError("invalid_credential", "credential is required")
	}
	return d.accounts.Import(ctx, AccountImportInput{
		Format:     input.Format,
		Name:       name,
		Provider:   providerID,
		Region:     region,
		Enabled:    true,
		Credential: payload,
	}, payload)
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
	usd := input.CreditUSD
	if usd <= 0 {
		usd = donationDefaultUSD
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
	d.sweepLocked()
	d.sessionStore()[record.ID] = record
	d.mu.Unlock()
	return *record, nil
}

// PollSession reports progress on a web-authorized contribution. Once the
// provider login reports done it enables the account and credits the
// contributor exactly once.
func (d *Donations) PollSession(ctx context.Context, sessionID string) (DonationSession, error) {
	d.mu.Lock()
	d.sweepLocked()
	record, ok := d.sessionStore()[sessionID]
	d.mu.Unlock()
	if !ok {
		return DonationSession{}, operationError("not_found", "unknown donation session")
	}

	// Already settled: report the stored outcome without crediting again.
	if record.Status == "credited" || record.Status == "failed" {
		return *record, nil
	}

	done, message, err := d.accounts.PollLogin(ctx, record.AccountID)
	if err != nil {
		return *record, operationError("login_poll_failed", err.Error())
	}
	record.Message = message
	if !done {
		return *record, nil
	}

	return *d.settle(ctx, record), nil
}

// CancelSession abandons a pending contribution and removes its placeholder
// account. A settled session is left alone so a finished reward is not lost.
func (d *Donations) CancelSession(ctx context.Context, sessionID string) error {
	d.mu.Lock()
	record, ok := d.sessionStore()[sessionID]
	if ok && (record.Status == "credited" || record.Status == "failed") {
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
func (d *Donations) settle(ctx context.Context, record *DonationSession) *DonationSession {
	// Enable+boot so the authorized credential is actually in the pool.
	enabled := true
	if _, err := d.accounts.Update(ctx, record.AccountID, accounts.UpdateAccount{Enabled: &enabled}); err != nil {
		record.Status = "failed"
		record.Message = "authorization succeeded but the account could not be enabled: " + err.Error()
		return record
	}

	quota := QuotaForUSD(record.CreditUSD)
	record.CreditedQuota = quota
	if err := d.creditQuota(ctx, record.NewAPIUserID, quota); err != nil {
		// The account is valid and stays in the pool; only the reward is
		// outstanding, so an operator can credit it without re-authorizing.
		record.Status = "failed"
		record.CreditError = err.Error()
		record.Message = "account authorized and added, but the credit failed"
		return record
	}
	record.Status = "credited"
	record.Credited = true
	record.Message = "authorized and credited"
	return record
}

// sweepLocked drops expired pending sessions and their placeholder accounts.
// Must be called with d.mu held. It never touches a settled session, so a
// completed reward is reported even after the TTL.
func (d *Donations) sweepLocked() {
	if d.sessions == nil {
		return
	}
	cutoff := d.now().Add(-donationSessionTTL)
	for id, record := range d.sessions {
		if record.Status != "pending" || record.CreatedAt.After(cutoff) {
			continue
		}
		delete(d.sessions, id)
		if d.accounts != nil {
			go func(accountID string) {
				_ = d.accounts.Delete(context.Background(), accountID)
			}(record.AccountID)
		}
	}
}
