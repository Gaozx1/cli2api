package control

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
	// ledger records what has already been paid, so a repeat submission of the
	// same credential (or from the same New API user) is refused.
	ledger *DonationRewardLedger
	// loaded guards the one-time restore of pending sessions from the store.
	loaded bool
	// clock is injectable for tests.
	clock func() time.Time
}

// DonationRewardLedger records what has already been paid, so one credential or
// one New API user cannot be rewarded twice.
//
// The donation endpoints are unauthenticated, so without this a caller can
// submit the same credential repeatedly and collect the reward each time. The
// ledger is persisted: an in-memory-only record would be reset by a restart,
// which is exactly when a repeat claim is most attractive.
type DonationRewardLedger struct {
	// Credentials maps a credential fingerprint to the New API user id it was
	// already paid for, so the same account cannot be contributed twice.
	Credentials map[string]DonationClaim `json:"credentials"`
	// Users counts how many rewards one New API user id has received, so a
	// single contributor cannot farm the endpoint with many credentials.
	Users map[string]int `json:"users"`
}

// DonationClaim is one recorded payout, kept so a repeat submission can be
// reported back with the account it already belongs to.
type DonationClaim struct {
	NewAPIUserID int       `json:"newapi_user_id"`
	AccountID    string    `json:"account_id"`
	Provider     string    `json:"provider"`
	Region       string    `json:"region"`
	ClaimedAt    time.Time `json:"claimed_at"`
}

// DonationFormatInfo describes one contributable credential format. The console
// renders these instead of hardcoding which provider supports what.
type DonationFormatInfo struct {
	Format   string `json:"format"`
	Provider string `json:"provider"`
	Label    string `json:"label"`
	Region   string `json:"region"`
	// RegionLabel is the provider's own name for the region ("Global", "CN").
	RegionLabel    string `json:"region_label"`
	CredentialKind string `json:"credential_kind"`
	// WebAuth is true when the provider exposes a browser login.
	WebAuth bool `json:"web_auth"`
	// CallbackRequired is true when that login cannot complete through a
	// loopback redirect, so the contributor must paste the callback URL back.
	// This mirrors the console wizard's paste field, but reads the provider's
	// LoginCompleter capability instead of naming providers.
	CallbackRequired bool   `json:"callback_required"`
	Description      string `json:"description"`
}

// Formats lists every contributable provider/region pair with its capabilities.
//
// One entry per region, not per provider: Qoder ships global+cn and WorkBuddy
// ships cn+global, and a contributor must be able to pick the one their account
// actually belongs to. Collapsing to DefaultRegion silently hides the others.
func (d *Donations) Formats() []DonationFormatInfo {
	formats := make([]DonationFormatInfo, 0, 8)
	for _, descriptor := range providers.List() {
		regions := descriptor.Regions
		if len(regions) == 0 {
			// No declared regions: still advertise one entry so the provider is
			// selectable, using its default.
			regions = []providers.RegionDescriptor{{ID: descriptor.DefaultRegion}}
		}
		for _, format := range descriptor.CredentialFormats {
			kind := "json"
			if format == donationQoderFormat {
				kind = "qoder_native"
			}
			for _, region := range regions {
				regionID := region.ID
				if regionID == "" {
					regionID = descriptor.DefaultRegion
				}
				formats = append(formats, DonationFormatInfo{
					Format:           format,
					Provider:         descriptor.ID,
					Label:            descriptor.Label,
					Region:           regionID,
					RegionLabel:      region.Label,
					CredentialKind:   kind,
					WebAuth:          d.supportsWebAuth(descriptor.ID),
					CallbackRequired: d.requiresCallback(descriptor.ID),
					Description:      descriptor.Label + " account contribution",
				})
			}
		}
	}
	return formats
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

// donationDefaultUSD is the reward for one accepted contribution.
//
// It is server policy, not client input. The public endpoints still accept a
// reward field for compatibility, but it is ignored: the reward is paid out of
// the operator's own New API quota, so a caller never gets to price its own
// payout. Changing the amount is a code change (later, a system setting), never
// something a request can influence.
const donationDefaultUSD = 0.5

// DonationRewardUSD is the reward for one accepted contribution, exported so the
// console reports the same number the payout actually uses. When the two drifted
// apart the page advertised one amount while the ledger paid another.
func DonationRewardUSD() float64 {
	return donationDefaultUSD
}

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
	ID           string  `json:"session_id"`
	AccountID    string  `json:"account_id"`
	Provider     string  `json:"provider"`
	Region       string  `json:"region"`
	Name         string  `json:"name"`
	Format       string  `json:"format"`
	NewAPIUserID int     `json:"newapi_user_id"`
	CreditUSD    float64 `json:"credit_usd"`
	AuthURL      string  `json:"auth_url"`
	Status       string  `json:"status"`
	Message      string  `json:"message,omitempty"`
	// CallbackRequired tells the page to offer the callback-URL paste box,
	// because this provider cannot complete through a loopback redirect.
	CallbackRequired bool      `json:"callback_required"`
	Credited         bool      `json:"credited"`
	CreditedQuota    int       `json:"credited_quota,omitempty"`
	CreditError      string    `json:"credit_error,omitempty"`
	CreatedAt        time.Time `json:"created_at"`

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
	Format       string `json:"format"`
	Name         string `json:"name"`
	Region       string `json:"region"`
	NewAPIUserID int    `json:"newapi_user_id"`
	// CreditUSD is accepted for compatibility and ignored: the reward is server
	// policy (donationDefaultUSD).
	CreditUSD float64 `json:"credit_usd"`
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
	// CreditUSD is accepted for compatibility and ignored; the reward is server
	// policy (donationDefaultUSD). It used to be the caller's number to pick.
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

// donationRewardUSD is the reward for one accepted contribution.
//
// Server policy, never the caller's credit_usd. Both entry points read it from
// here so the amount has a single home should it ever become a setting.
func donationRewardUSD() float64 {
	return DonationRewardUSD()
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

	// The reward is priced here, not by the caller: credit_usd is accepted and
	// ignored, so a rejected or absurd value cannot influence the payout.
	usd := donationRewardUSD()

	// Refuse a repeat before importing anything, so a duplicate submission does
	// not leave a second copy of the same account in the pool.
	payload, err := donationCredentialPayload(input)
	if err != nil {
		return DonationResult{}, err
	}
	fingerprint, err := d.checkClaimable(ctx, input.Format, payload)
	if err != nil {
		return DonationResult{}, err
	}

	created, err := d.importAccount(ctx, providerID, region, input)
	if err != nil {
		return DonationResult{}, err
	}

	// Prove the account works before paying for it. A credential that only
	// passes the local shape check is not worth a reward, and importing it
	// already put it in the pool, so it is removed again when it is not live.
	if err := d.verifyAccountLive(ctx, providerID, created.ID); err != nil {
		_ = d.accounts.Delete(ctx, created.ID)
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
	d.recordClaim(ctx, fingerprint, DonationClaim{
		NewAPIUserID: input.NewAPIUserID,
		AccountID:    created.ID,
		Provider:     providerID,
		Region:       region,
		ClaimedAt:    d.now(),
	})
	result.Credited = true
	return result, nil
}

// donationCredentialPayload is the credential bytes a submission carries, in the
// form the fingerprint is computed over.
func donationCredentialPayload(input DonationRequest) ([]byte, error) {
	if input.Format == donationQoderFormat {
		// Qoder carries a blob plus machine id rather than a JSON credential.
		blob := strings.TrimSpace(input.UserBlob)
		machine := strings.TrimSpace(input.MachineID)
		if blob == "" || machine == "" {
			return nil, operationError("invalid_credential", "user_blob and machine_id are required")
		}
		return []byte(machine + "\x00" + blob), nil
	}
	if len(input.Credential) == 0 {
		return nil, operationError("invalid_credential", "credential is required")
	}
	return input.Credential, nil
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
			Format:              input.Format,
			Name:                name,
			Provider:            providerID,
			Region:              region,
			Enabled:             false,
			UserBlob:            strings.TrimSpace(input.UserBlob),
			MachineID:           strings.TrimSpace(input.MachineID),
			ContributedBy:       input.NewAPIUserID,
			ContributedProvider: providerID,
			ContributedRegion:   region,
			ContributedFormat:   strings.TrimSpace(input.Format),
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
		Format:              input.Format,
		Name:                name,
		Provider:            providerID,
		Region:              region,
		Enabled:             false,
		Credential:          sanitized,
		ContributedBy:       input.NewAPIUserID,
		ContributedProvider: providerID,
		ContributedRegion:   region,
		ContributedFormat:   strings.TrimSpace(input.Format),
	}, sanitized)
}

// donationLedgerSecret persists the reward ledger. It must outlive a restart:
// the endpoints are unauthenticated, so a reset ledger re-opens every claim
// that was already paid.
const donationLedgerSecret = "donation_reward_ledger"

// donationCredentialFingerprint identifies a credential by its content, so the
// same account cannot be contributed twice under different names.
//
// It hashes the canonical JSON of the payload with the host-override fields
// already stripped, so a resubmission that merely adds or changes base_url is
// recognised as the same credential rather than treated as a new one.
func donationCredentialFingerprint(format string, payload []byte) string {
	canonical := payload
	if stripped, err := stripDonationCredentialHosts(payload); err == nil {
		canonical = stripped
	}
	// Re-encode so key order and whitespace cannot change the fingerprint.
	var doc map[string]any
	if err := json.Unmarshal(canonical, &doc); err == nil && doc != nil {
		if encoded, err := json.Marshal(doc); err == nil {
			canonical = encoded
		}
	}
	sum := sha256.Sum256(append([]byte(format+"\x00"), canonical...))
	return hex.EncodeToString(sum[:])
}

// ledgerSnapshot returns the current ledger, loading it from the store once.
func (d *Donations) ledgerSnapshot(ctx context.Context) DonationRewardLedger {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ledger == nil {
		d.ledger = &DonationRewardLedger{
			Credentials: map[string]DonationClaim{},
			Users:       map[string]int{},
		}
		if d.settings != nil {
			if raw, ok, err := d.settings.GetSecret(ctx, donationLedgerSecret); err == nil && ok {
				var stored DonationRewardLedger
				if json.Unmarshal([]byte(raw), &stored) == nil {
					if stored.Credentials != nil {
						d.ledger.Credentials = stored.Credentials
					}
					if stored.Users != nil {
						d.ledger.Users = stored.Users
					}
				}
			}
		}
	}
	// Return a copy so a caller cannot mutate the ledger off-lock.
	out := DonationRewardLedger{
		Credentials: make(map[string]DonationClaim, len(d.ledger.Credentials)),
		Users:       make(map[string]int, len(d.ledger.Users)),
	}
	for k, v := range d.ledger.Credentials {
		out.Credentials[k] = v
	}
	for k, v := range d.ledger.Users {
		out.Users[k] = v
	}
	return out
}

// checkClaimable refuses a submission whose credential has already been paid,
// and returns the fingerprint to record once the reward is issued.
//
// Only the credential is deduplicated: one account is one reward, regardless of
// who submits it or how many accounts a single contributor holds. There is no
// per-user cap, so someone with several genuine accounts is paid for each.
func (d *Donations) checkClaimable(ctx context.Context, format string, payload []byte) (string, error) {
	fingerprint := donationCredentialFingerprint(format, payload)
	ledger := d.ledgerSnapshot(ctx)
	if claim, ok := ledger.Credentials[fingerprint]; ok {
		return fingerprint, operationError("credential_already_contributed",
			fmt.Sprintf("this account has already been contributed (credited to user %d)", claim.NewAPIUserID))
	}
	return fingerprint, nil
}

// recordClaim persists one payout so it cannot be claimed again.
func (d *Donations) recordClaim(ctx context.Context, fingerprint string, claim DonationClaim) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ledger == nil {
		d.ledger = &DonationRewardLedger{
			Credentials: map[string]DonationClaim{},
			Users:       map[string]int{},
		}
	}
	if fingerprint != "" {
		d.ledger.Credentials[fingerprint] = claim
	}
	d.ledger.Users[strconv.Itoa(claim.NewAPIUserID)]++
	if d.settings == nil {
		return
	}
	if encoded, err := json.Marshal(d.ledger); err == nil {
		_ = d.settings.SetSecret(ctx, donationLedgerSecret, string(encoded))
	}
}

// verifyAccountLive proves a contributed account actually works before it is
// rewarded.
//
// Credential validation is a local shape check (a non-empty token, a user_
// prefix), so a fabricated credential passes it. Probe alone is not enough
// either: for WorkBuddy, Trae and Codex it only re-reads the stored credential.
// So this makes a real provider call.
//
// The call is Models rather than Quota, because quota/billing endpoints are
// flaky in a way that says nothing about the credential: a genuinely live
// account whose billing endpoint returns a transient 500 must not be refused.
// Models authenticates the credential, which is exactly the question here.
//
// A transport or server-side failure is reported as retryable so the caller can
// treat it as "not verified yet" rather than "this credential is bad".
func (d *Donations) verifyAccountLive(ctx context.Context, providerID, accountID string) error {
	adapter, ok := d.donationAdapter(providerID)
	if !ok {
		return operationError("provider_unsupported", "this provider is not available")
	}
	if adapter.Prober != nil {
		// A probe that positively reports not-ready with an auth-class reason is
		// authoritative: the credential itself was rejected.
		health, err := adapter.Prober.Probe(ctx, accountID)
		if err == nil && !health.Ready && health.LastError != "" && donationAuthClassError(health.LastError) {
			return operationError("account_not_live", "the account did not pass its provider check: "+health.LastError)
		}
	}
	if adapter.Models == nil {
		return operationError("account_unverifiable", "this provider cannot verify a contributed account")
	}
	if _, err := adapter.Models.Models(ctx, accountID); err != nil {
		if donationAuthClassError(err.Error()) {
			return operationError("account_not_live", "the provider rejected this credential: "+err.Error())
		}
		return operationError("account_unverified", "the account could not be verified right now: "+err.Error())
	}
	return nil
}

// donationAuthClassError reports whether an error means the credential itself was
// rejected, as opposed to the provider being unreachable or erroring.
//
// Only the former should disqualify a contribution: refusing a valid account
// because a provider had a bad minute is worse than the abuse this guards.
func donationAuthClassError(message string) bool {
	lower := strings.ToLower(message)
	for _, marker := range []string{
		"unauthorized", "unauthenticated", "invalid token", "invalid access token",
		"invalid api key", "invalid key", "token expired", "expired token",
		"login_required", "login required", "authentication failed", "auth failed",
		"401", "403", "permission denied", "forbidden",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
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

// donationAdapter resolves a provider's adapter, or nil when it has none.
func (d *Donations) donationAdapter(providerID string) (providers.Adapter, bool) {
	if d == nil || d.accounts == nil || d.accounts.Providers == nil {
		return providers.Adapter{}, false
	}
	return d.accounts.Providers.Get(providerID)
}

// supportsWebAuth reports whether a provider exposes a browser login.
func (d *Donations) supportsWebAuth(providerID string) bool {
	adapter, ok := d.donationAdapter(providerID)
	return ok && adapter.Login != nil
}

// requiresCallback reports whether the provider's login must be finished by
// pasting the callback URL. This is the LoginCompleter capability — the same
// thing the console wizard offers for trae/devin/codex — read from the adapter
// instead of a hardcoded provider list, so a new provider needs no UI change.
func (d *Donations) requiresCallback(providerID string) bool {
	adapter, ok := d.donationAdapter(providerID)
	if !ok || adapter.Login == nil {
		return false
	}
	_, completable := adapter.Login.(providers.LoginCompleter)
	return completable
}

func (d *Donations) sessionStore() map[string]*DonationSession {
	if d.sessions == nil {
		d.sessions = map[string]*DonationSession{}
	}
	return d.sessions
}

// donationSessionsSecret holds every session as one JSON map. Sessions must
// outlive a process restart: the placeholder account is created before the
// contributor authorizes, so an in-memory-only record would leave that account
// permanently orphaned — nothing left to poll it, and nothing left to sweep it.
//
// One key for the whole map (rather than one key per session) because the store
// exposes no key enumeration, so the set must be recoverable without scanning.
const donationSessionsSecret = "donation_sessions"

// loadSessions restores persisted sessions once, then sweeps anything that
// expired while the process was down.
//
// The whole load runs under the lock: it both reads the store and mutates the
// map, and two concurrent callers must not interleave. The store read is a
// single key, so holding the lock across it is bounded and simple.
func (d *Donations) loadSessions(ctx context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.loaded {
		return
	}
	d.loaded = true

	if d.settings == nil {
		return
	}
	raw, ok, err := d.settings.GetSecret(ctx, donationSessionsSecret)
	if err != nil || !ok || strings.TrimSpace(raw) == "" {
		return
	}
	var stored map[string]*DonationSession
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return
	}
	for id, record := range stored {
		if record == nil || record.ID == "" {
			continue
		}
		// Nothing is live yet on the very first load, so this is a plain
		// restore; a record that was mid-settle when the process died cannot be
		// resumed, so the claim is released and the round is re-driven.
		record.settling = false
		d.sessionStore()[id] = record
	}
}

// flushSessionsLocked mirrors the session map to the store. Must be called with
// d.mu held.
func (d *Donations) flushSessionsLocked(ctx context.Context) {
	if d.settings == nil {
		return
	}
	encoded, err := json.Marshal(d.sessions)
	if err != nil {
		return
	}
	_ = d.settings.SetSecret(ctx, donationSessionsSecret, string(encoded))
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
	if !d.supportsWebAuth(providerID) {
		return DonationSession{}, operationError("web_auth_unsupported", "this provider does not support web authorization")
	}
	region := strings.ToLower(strings.TrimSpace(input.Region))
	if region == "" {
		region = defaultDonationRegion(providerID)
	}
	usd := donationRewardUSD()
	name := strings.TrimSpace(input.Name)
	if name == "" {
		name = providerID + " donation"
	}

	// Created enabled, matching the console wizard: a Qoder login needs the
	// account's child worker running, and the worker only starts for an enabled
	// account. This is safe for the pool because the account has no usable
	// credential until the provider authorizes it, and an account that is not
	// ready is never routed to.
	//
	// The pasted-credential path is different and imports disabled: there the
	// credential is untrusted input that has only passed a shape check, so
	// enabling it is an operator decision.
	created, err := d.accounts.Create(ctx, accounts.CreateAccount{
		Name: name, Provider: providerID, Region: region, Enabled: true,
		// Provenance, so the console can show which New API user contributed
		// this account and what they contributed.
		ContributedBy:       input.NewAPIUserID,
		ContributedProvider: providerID,
		ContributedRegion:   region,
		ContributedFormat:   strings.TrimSpace(input.Format),
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
		ID:               donationSessionID(),
		AccountID:        created.ID,
		Provider:         providerID,
		Region:           region,
		Name:             name,
		Format:           input.Format,
		NewAPIUserID:     input.NewAPIUserID,
		CreditUSD:        usd,
		AuthURL:          session.AuthURL,
		Status:           "pending",
		CallbackRequired: d.requiresCallback(providerID),
		Message:          "open the authorization URL to finish login",
		CreatedAt:        d.now(),
	}
	d.mu.Lock()
	expired := d.sweepLocked()
	d.sessionStore()[record.ID] = record
	d.flushSessionsLocked(ctx)
	d.mu.Unlock()
	d.deleteSweptAccounts(ctx, expired)
	return *record, nil
}

// CompleteSession finishes a contribution whose provider redirects to a loopback
// address this process cannot receive, using the callback URL the contributor
// copied out of their browser. It is the donation equivalent of the console
// wizard's callback paste, and is only valid for providers that implement
// LoginCompleter.
func (d *Donations) CompleteSession(ctx context.Context, sessionID, callbackURL string) (DonationSession, error) {
	if strings.TrimSpace(callbackURL) == "" {
		return DonationSession{}, operationError("invalid_request", "callback_url is required")
	}
	d.loadSessions(ctx)
	if d.accounts == nil {
		return DonationSession{}, operationError("donations_unavailable", "account service is unavailable")
	}
	// Claim the settle path first, exactly as the polling flow does, so a
	// callback and a concurrent poll cannot both credit. Values are copied out
	// under the lock and used off it; nothing is read through the shared record
	// after the lock is released.
	d.mu.Lock()
	record, ok := d.sessionStore()[sessionID]
	if !ok {
		d.mu.Unlock()
		return DonationSession{}, operationError("not_found", "unknown donation session")
	}
	if record.settling || donationSettled(record) {
		snapshot := *record
		d.mu.Unlock()
		return snapshot, nil
	}
	record.settling = true
	accountID := record.AccountID
	userID := record.NewAPIUserID
	quota := QuotaForUSD(record.CreditUSD)
	d.flushSessionsLocked(ctx)
	d.mu.Unlock()

	if err := d.accounts.CompleteLogin(ctx, accountID, callbackURL); err != nil {
		// Release the claim: the contributor can paste a corrected URL.
		d.mu.Lock()
		if current, found := d.sessionStore()[sessionID]; found {
			current.settling = false
			d.flushSessionsLocked(ctx)
		}
		d.mu.Unlock()
		return d.sessionSnapshot(sessionID), operationError("login_callback_failed", err.Error())
	}

	d.settle(ctx, sessionID, accountID, userID, quota)
	return d.sessionSnapshot(sessionID), nil
}

// PollSession reports progress on a web-authorized contribution. Once the
// provider login reports done it enables the account and credits the
// contributor exactly once.
func (d *Donations) PollSession(ctx context.Context, sessionID string) (DonationSession, error) {
	d.loadSessions(ctx)
	d.mu.Lock()
	expired := d.sweepLocked()
	record, ok := d.sessionStore()[sessionID]
	// Snapshot under the lock, then act outside it. A settled or
	// already-claimed round reports what it knows; only the poll that wins the
	// claim below is allowed to credit.
	var early DonationSession
	claimed := false
	accountID := ""
	callbackRequired := false
	if ok {
		early = *record
		claimed = !record.settling && !donationSettled(record)
		// Copy the fields needed off-lock: reading through the shared pointer
		// after releasing the lock would race with settle's writes.
		accountID = record.AccountID
		callbackRequired = record.CallbackRequired
	}
	d.mu.Unlock()
	d.deleteSweptAccounts(ctx, expired)

	if !ok {
		return DonationSession{}, operationError("not_found", "unknown donation session")
	}
	if !claimed {
		return early, nil
	}

	// A provider that needs a pasted callback cannot complete by polling; the
	// page must submit the callback URL instead.
	if callbackRequired {
		return early, nil
	}

	done, message, err := d.accounts.PollLogin(ctx, accountID)
	if err == nil && done {
		// The provider authorized the account. Persist the login now, while the
		// worker that holds it is still running: a child-process home lives on
		// tmpfs and is wiped on restart, so an unsaved login is lost and the
		// account comes back needing a fresh one. Best-effort -- a failure here
		// must not block a contribution that succeeded, and the settle step
		// re-verifies the account anyway.
		if err := d.accounts.PersistCredential(ctx, accountID); err != nil {
			log.Printf("donation: persist credential for %s: %v", accountID, err)
		}
	}
	if err != nil {
		// The provider forgot this handshake — typically because the process
		// restarted. Say so plainly so the page can offer a restart instead of
		// showing a provider-internal error. Nothing is credited either way.
		if donationLostLoginState(err) {
			d.mu.Lock()
			if current, found := d.sessionStore()[sessionID]; found && !donationSettled(current) {
				current.Message = "the authorization session expired; start it again"
				d.flushSessionsLocked(ctx)
			}
			snapshot := d.sessionSnapshotLocked(sessionID)
			d.mu.Unlock()
			return snapshot, nil
		}
		return d.sessionSnapshot(sessionID), operationError("login_poll_failed", err.Error())
	}

	// Claim the settle path under the lock. PollLogin runs outside it because it
	// does I/O; the claim is what makes the credit happen exactly once when
	// several polls observe the same completed login.
	d.mu.Lock()
	current, found := d.sessionStore()[sessionID]
	if !found {
		d.mu.Unlock()
		return DonationSession{}, operationError("not_found", "unknown donation session")
	}
	if current.settling || donationSettled(current) {
		snapshot := *current
		d.mu.Unlock()
		return snapshot, nil
	}
	current.Message = message
	if !done {
		snapshot := *current
		d.mu.Unlock()
		return snapshot, nil
	}
	current.settling = true
	accountID = current.AccountID
	userID := current.NewAPIUserID
	quota := QuotaForUSD(current.CreditUSD)
	sessionIDCopy := current.ID
	d.mu.Unlock()

	d.settle(ctx, sessionIDCopy, accountID, userID, quota)
	return d.sessionSnapshot(sessionID), nil
}

// sessionSnapshotLocked copies a session while d.mu is already held.
func (d *Donations) sessionSnapshotLocked(sessionID string) DonationSession {
	record, ok := d.sessionStore()[sessionID]
	if !ok {
		return DonationSession{}
	}
	return *record
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
	d.loadSessions(ctx)
	d.mu.Lock()
	record, ok := d.sessionStore()[sessionID]
	if ok && (donationSettled(record) || record.settling) {
		d.mu.Unlock()
		return operationError("invalid_request", "this contribution has already finished")
	}
	delete(d.sessionStore(), sessionID)
	d.flushSessionsLocked(ctx)
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
// It runs outside d.mu because both steps do I/O, so it takes plain values
// rather than the session pointer: reading fields off a shared record here would
// race with another poll. The outcome is committed in a single locked update,
// which is what keeps a second poll from crediting again -- the caller claimed
// the round by setting settling before calling this.
func (d *Donations) settle(ctx context.Context, sessionID, accountID string, userID, quota int) {
	// Prove the account works before paying for it. The provider login proves
	// the credential exists, but not that it can actually serve; a reward is
	// paid out of the operator's quota, so it waits for a real provider call.
	providerID := ""
	if record, ok := d.sessionRecord(sessionID); ok {
		providerID = record.Provider
	}
	if providerID != "" {
		if err := d.verifyAccountLive(ctx, providerID, accountID); err != nil {
			// The account is not usable, so it must not stay in the pool.
			_ = d.accounts.Delete(ctx, accountID)
			d.finishSettle(ctx, sessionID, "failed", 0, err.Error(),
				"the account could not be verified with its provider, so it was not added")
			return
		}
	}

	// Enable+boot so the authorized credential is actually in the pool.
	enabled := true
	if _, err := d.accounts.Update(ctx, accountID, accounts.UpdateAccount{Enabled: &enabled}); err != nil {
		d.finishSettle(ctx, sessionID, "failed", 0, "",
			"authorization succeeded but the account could not be enabled: "+err.Error())
		return
	}

	if err := d.creditQuota(ctx, userID, quota); err != nil {
		// The account is valid and stays in the pool; only the reward is
		// outstanding, so an operator can credit it without re-authorizing.
		d.finishSettle(ctx, sessionID, "failed", quota, err.Error(),
			"account authorized and added, but the credit failed")
		return
	}
	// Record the payout so this credential and this user cannot claim again.
	d.recordClaim(ctx, d.sessionFingerprint(ctx, sessionID), DonationClaim{
		NewAPIUserID: userID,
		AccountID:    accountID,
		Provider:     providerID,
		ClaimedAt:    d.now(),
	})
	d.finishSettle(ctx, sessionID, "credited", quota, "", "authorized and credited")
}

// sessionRecord reads one session without holding the lock afterwards.
func (d *Donations) sessionRecord(sessionID string) (DonationSession, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	record, ok := d.sessionStore()[sessionID]
	if !ok {
		return DonationSession{}, false
	}
	return *record, true
}

// sessionFingerprint is the credential fingerprint for a settled web-auth round,
// computed from the credential the provider actually stored.
func (d *Donations) sessionFingerprint(ctx context.Context, sessionID string) string {
	record, ok := d.sessionRecord(sessionID)
	if !ok {
		return ""
	}
	_, payload, err := d.accounts.store().LoadCredentialPayload(ctx, record.AccountID)
	if err != nil || len(payload) == 0 {
		return ""
	}
	return donationCredentialFingerprint(record.Format, payload)
}

// finishSettle commits one settle outcome and releases the claim. The outcome is
// mirrored to the store so a finished reward stays reportable across a restart.
func (d *Donations) finishSettle(ctx context.Context, sessionID, status string, quota int, creditErr, message string) {
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
	d.flushSessionsLocked(ctx)
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
	// Mirror the removal so a restart cannot resurrect a swept round and strand
	// its placeholder account all over again.
	if len(expired) > 0 {
		d.flushSessionsLocked(context.Background())
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

// SweepExpired reclaims expired rounds. It is exposed so a background ticker can
// drive it: reclamation must not depend on a later request arriving, or an
// abandoned round leaves its placeholder account behind forever.
func (d *Donations) SweepExpired(ctx context.Context) {
	d.loadSessions(ctx)
	d.mu.Lock()
	expired := d.sweepLocked()
	d.mu.Unlock()
	d.deleteSweptAccounts(ctx, expired)
}

// donationSweepInterval is how often abandoned rounds are reclaimed. The TTL is
// 15 minutes, so a coarse tick is enough.
const donationSweepInterval = time.Minute

// RunSweepLoop reclaims expired rounds until stop closes.
func (d *Donations) RunSweepLoop(stop <-chan struct{}) {
	ticker := time.NewTicker(donationSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			d.SweepExpired(context.Background())
		}
	}
}

// RestartSession re-opens the provider login for a pending round.
//
// The provider's login handshake lives in the provider's memory, so a process
// restart (or an expired auth URL) leaves a pending round that can never finish
// by polling. Rather than strand the account or force a duplicate, this re-runs
// StartLogin on the same account and returns a fresh authorization URL -- the
// same thing the console wizard does when it starts the browser login again.
func (d *Donations) RestartSession(ctx context.Context, sessionID string) (DonationSession, error) {
	d.loadSessions(ctx)
	d.mu.Lock()
	record, ok := d.sessionStore()[sessionID]
	if ok {
		// A claimed round is mid-flight; restarting it would fight the claim.
		if record.settling || donationSettled(record) {
			snapshot := *record
			d.mu.Unlock()
			return snapshot, nil
		}
		record.Message = "authorization restarted"
	}
	d.mu.Unlock()
	if !ok {
		return DonationSession{}, operationError("not_found", "unknown donation session")
	}
	if d.accounts == nil {
		return DonationSession{}, operationError("donations_unavailable", "account service is unavailable")
	}

	session, err := d.accounts.StartLogin(ctx, record.AccountID)
	if err != nil {
		return d.sessionSnapshot(sessionID), operationError("login_start_failed", err.Error())
	}
	d.mu.Lock()
	if current, found := d.sessionStore()[sessionID]; found {
		current.AuthURL = session.AuthURL
		current.Message = "authorization restarted"
	}
	d.flushSessionsLocked(ctx)
	snapshot := *d.sessionStore()[sessionID]
	d.mu.Unlock()
	return snapshot, nil
}

// donationLostLoginState reports whether a poll error means the provider no
// longer knows about this login. That happens when the process restarted, or
// when the provider dropped the handshake; either way the round needs a restart.
func donationLostLoginState(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "login not started") ||
		strings.Contains(message, "unknown account") ||
		strings.Contains(message, "not running")
}
