package control

import (
	"context"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// A provider offered in more than one region must list one entry per region.
//
// Qoder ships global+cn and WorkBuddy ships cn+global. Emitting only the
// provider's default silently hides the other region from the contributor,
// who then cannot contribute the account they actually hold.
func TestFormatsListEveryRegion(t *testing.T) {
	donations, _, _ := donationHarness(t, &fakeLogin{}, nil)
	formats := donations.Formats()

	type key struct{ provider, region string }
	seen := map[key]int{}
	for _, f := range formats {
		seen[key{f.Provider, f.Region}]++
	}

	// Every declared region of every provider must appear.
	for _, descriptor := range providers.List() {
		for _, region := range descriptor.Regions {
			k := key{descriptor.ID, region.ID}
			if seen[k] == 0 {
				t.Errorf("provider %q region %q is missing from the donation formats", descriptor.ID, region.ID)
			}
			if seen[k] > 1 {
				t.Errorf("provider %q region %q appears %d times", descriptor.ID, region.ID, seen[k])
			}
		}
	}

	// The two multi-region providers are the reason this matters.
	for _, want := range []key{{"qoder", "global"}, {"qoder", "cn"}, {"workbuddy", "cn"}, {"workbuddy", "global"}} {
		if seen[want] == 0 {
			t.Errorf("expected %s/%s to be contributable", want.provider, want.region)
		}
	}
}

// Each entry must carry the region's own label so the picker can distinguish
// two entries of the same provider.
func TestFormatsCarryRegionLabel(t *testing.T) {
	donations, _, _ := donationHarness(t, &fakeLogin{}, nil)
	for _, f := range donations.Formats() {
		if f.Region == "" {
			t.Errorf("format %s/%s has no region", f.Provider, f.Format)
		}
		if f.RegionLabel == "" {
			t.Errorf("format %s/%s has no region label", f.Provider, f.Region)
		}
	}
}

// A contributed region must be honoured: the session records the region the
// contributor picked, not the provider default.
func TestStartSessionHonoursExplicitRegion(t *testing.T) {
	login := &fakeLogin{authURL: "https://provider.example/auth"}
	donations, store, _ := donationHarness(t, login, nil)
	// The harness registers workbuddy; add qoder so its cn region is usable.
	donations.accounts.Providers.Register(providers.Adapter{ID: "qoder", Login: login})

	session, err := donations.StartSession(context.Background(), DonationStart{
		Format:       "qoder-native-v1",
		Region:       "cn",
		NewAPIUserID: 7,
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if session.Region != "cn" {
		t.Fatalf("session region = %q, want cn (the contributor's pick)", session.Region)
	}
	if got := store.accounts[session.AccountID].ProviderRegion; got != "cn" {
		t.Fatalf("account region = %q, want cn", got)
	}
}

// An omitted region still falls back to the provider default.
func TestStartSessionDefaultsRegion(t *testing.T) {
	login := &fakeLogin{authURL: "https://provider.example/auth"}
	donations, store, _ := donationHarness(t, login, nil)

	session, err := donations.StartSession(context.Background(), DonationStart{
		Format:       "workbuddy-oauth-v1",
		NewAPIUserID: 7,
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if session.Region != "cn" {
		t.Fatalf("session region = %q, want workbuddy's default cn", session.Region)
	}
	if got := store.accounts[session.AccountID].ProviderRegion; got != "cn" {
		t.Fatalf("account region = %q, want cn", got)
	}
}
