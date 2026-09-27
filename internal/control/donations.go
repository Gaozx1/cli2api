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

// DonationRequest is one contributed account plus the contributor identity.
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
	// canonical flat credential; for Qoder the raw user blob and machine id.
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
