package control

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestQuotaForUSD(t *testing.T) {
	cases := []struct {
		usd  float64
		want int
	}{
		{0, 0},
		{-1, 0},
		{1, 500000},
		{2.5, 1250000},
	}
	for _, tc := range cases {
		if got := QuotaForUSD(tc.usd); got != tc.want {
			t.Fatalf("QuotaForUSD(%v) = %d, want %d", tc.usd, got, tc.want)
		}
	}
}

func TestDonationProviderMapsFormats(t *testing.T) {
	cases := map[string]string{
		"qoder-native-v1":    "qoder",
		"workbuddy-oauth-v1": "workbuddy",
		"trae-oauth-v1":      "trae",
		"nonsense-format":    "",
	}
	for format, want := range cases {
		got, err := donationProvider(format)
		if want == "" {
			if err == nil {
				t.Fatalf("format %q must be rejected", format)
			}
			continue
		}
		if err != nil {
			t.Fatalf("format %q: %v", format, err)
		}
		if got != want {
			t.Fatalf("format %q maps to %q, want %q", format, got, want)
		}
	}
}

func TestDefaultDonationRegionUsesProviderDefault(t *testing.T) {
	if got := defaultDonationRegion("workbuddy"); got != "cn" {
		t.Fatalf("workbuddy default region = %q, want cn", got)
	}
	if got := defaultDonationRegion("qoder"); got != "global" {
		t.Fatalf("qoder default region = %q, want global", got)
	}
}

// The credit is the only irreversible side effect, so its request shape is
// pinned: New API rejects the call outright when New-Api-User is missing or
// does not match the token's user.
func TestCreditQuotaSendsNewAPIUserHeader(t *testing.T) {
	var gotAuth, gotUser, gotBody, gotPath string
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotUser = r.Header.Get("New-Api-User")
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"message":""}`))
	}))
	defer site.Close()

	donations := &Donations{HTTP: site.Client(), BaseURL: site.URL, Token: "test-token"}
	if err := donations.creditQuota(context.Background(), 42, 500000); err != nil {
		t.Fatalf("creditQuota: %v", err)
	}
	if gotPath != "/api/user/manage" {
		t.Fatalf("path = %q, want /api/user/manage", gotPath)
	}
	if gotAuth != "Bearer test-token" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotUser != "42" {
		t.Fatalf("New-Api-User = %q, want 42", gotUser)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if payload["action"] != "add_quota" || payload["mode"] != "add" {
		t.Fatalf("unexpected action/mode: %v", payload)
	}
	if payload["value"].(float64) != 500000 {
		t.Fatalf("value = %v, want 500000", payload["value"])
	}
}

func TestCreditQuotaSurfacesBusinessFailure(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":false,"message":"额度变更量不能为0"}`))
	}))
	defer site.Close()

	donations := &Donations{HTTP: site.Client(), BaseURL: site.URL, Token: "t"}
	err := donations.creditQuota(context.Background(), 1, 0)
	if err == nil {
		t.Fatal("a site-level failure must surface as an error")
	}
	if !strings.Contains(err.Error(), "额度变更量不能为0") {
		t.Fatalf("error must carry the site message, got %v", err)
	}
}

func TestCreditQuotaRequiresConfiguration(t *testing.T) {
	donations := &Donations{settings: NewSettings(newFakeStore(&callLog{}))}
	if err := donations.creditQuota(context.Background(), 1, 100); err == nil {
		t.Fatal("an unconfigured site must not attempt a credit")
	}
}

// A contribution with no usable credential must fail before any credit is
// issued, so a caller can never be paid for an account the pool rejected.
func TestSubmitRejectsBeforeCredit(t *testing.T) {
	credited := false
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		credited = true
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer site.Close()

	donations := &Donations{HTTP: site.Client(), BaseURL: site.URL, Token: "t"}
	if _, err := donations.Submit(context.Background(), DonationRequest{
		Format:       "workbuddy-oauth-v1",
		NewAPIUserID: 7,
	}); err == nil {
		t.Fatal("a missing credential must be rejected")
	}
	if credited {
		t.Fatal("the site must not be credited when the import never ran")
	}
}

func TestSubmitRequiresNumericUserID(t *testing.T) {
	donations := &Donations{}
	if _, err := donations.Submit(context.Background(), DonationRequest{
		Format: "workbuddy-oauth-v1",
	}); err == nil {
		t.Fatal("a missing New API user id must be rejected")
	}
}

func TestSubmitRejectsUnsupportedFormat(t *testing.T) {
	donations := &Donations{}
	if _, err := donations.Submit(context.Background(), DonationRequest{
		Format:       "not-a-format",
		NewAPIUserID: 1,
	}); err == nil {
		t.Fatal("an unsupported format must be rejected")
	}
}

// The console must never echo the stored donation token back to a reader.
func TestSystemSettingsHidesDonationToken(t *testing.T) {
	store := newFakeStore(&callLog{})
	store.secrets[donationBaseURLSecret] = "https://api.example.test"
	store.secrets[donationTokenSecret] = "super-secret-token"
	system := &System{Settings: NewSettings(store), CrossProviderPool: &atomic.Bool{}}

	settings := system.Current(context.Background())
	if settings.DonationBaseURL != "https://api.example.test" {
		t.Fatalf("base url = %q", settings.DonationBaseURL)
	}
	if !settings.DonationConfigured {
		t.Fatal("a configured site must report configured")
	}
	encoded, _ := json.Marshal(settings)
	if strings.Contains(string(encoded), "super-secret-token") {
		t.Fatal("the donation token must not appear in system settings")
	}
}

func TestSystemSettingsRejectsNonHTTPSDonationURL(t *testing.T) {
	store := newFakeStore(&callLog{})
	system := &System{Settings: NewSettings(store), CrossProviderPool: &atomic.Bool{}}
	bad := "ftp://example.test"
	if err := system.Patch(context.Background(), SystemSettingsPatch{DonationBaseURL: &bad}); err == nil {
		t.Fatal("a non-http(s) donation URL must be rejected")
	}
	good := "https://api.example.test/"
	if err := system.Patch(context.Background(), SystemSettingsPatch{DonationBaseURL: &good}); err != nil {
		t.Fatalf("valid URL rejected: %v", err)
	}
	if got := store.secrets[donationBaseURLSecret]; got != "https://api.example.test" {
		t.Fatalf("stored base url = %q, want trailing slash trimmed", got)
	}
}
