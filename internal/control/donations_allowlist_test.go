package control

import (
	"context"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// The allow-list must be applied to what the page offers AND to what the
// endpoints accept: hiding a provider from the UI is not a policy if a caller
// can post its format directly.
func TestDonationAllowListGatesFormatsAndSubmit(t *testing.T) {
	donations, store, _ := donationHarness(t, &fakeLogin{}, nil)
	settings := NewSettings(store)
	donations.settings = settings
	ctx := context.Background()

	// With nothing stored, every provider is offered (historical behaviour).
	all := donations.Formats(ctx)
	if len(all) == 0 {
		t.Fatal("an unset allow-list must not hide every provider")
	}

	// Restrict to one provider that actually exists.
	target := all[0].Provider
	if err := settings.SetSecret(ctx, donationFormatsSecret, target); err != nil {
		t.Fatalf("set allow-list: %v", err)
	}

	filtered := donations.Formats(ctx)
	if len(filtered) == 0 {
		t.Fatalf("allow-list %q produced no formats", target)
	}
	for _, format := range filtered {
		if format.Provider != target {
			t.Fatalf("format %s/%s survived an allow-list of %q", format.Provider, format.Region, target)
		}
	}

	if !donations.DonationAllowed(ctx, target) {
		t.Fatalf("%s should be contributable", target)
	}
	// Every other registered provider must now be refused.
	for _, descriptor := range providers.List() {
		if descriptor.ID == target {
			continue
		}
		if donations.DonationAllowed(ctx, descriptor.ID) {
			t.Fatalf("%s must be refused while the allow-list names only %s", descriptor.ID, target)
		}
	}

	// Clearing the setting restores "everything".
	if err := settings.SetSecretOrEmpty(ctx, donationFormatsSecret, ""); err != nil {
		t.Fatalf("clear allow-list: %v", err)
	}
	if len(donations.Formats(ctx)) != len(all) {
		t.Fatal("clearing the allow-list must restore every provider")
	}
}

// A stale or hand-edited list may contain junk; it must be normalised, and an
// all-empty list must mean "everything" rather than "nothing".
func TestParseDonationAllowedProviders(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{",,", nil},
		{"workbuddy", []string{"workbuddy"}},
		{"WorkBuddy", []string{"workbuddy"}},
		{"workbuddy,qoder", []string{"workbuddy", "qoder"}},
		{" workbuddy , qoder ", []string{"workbuddy", "qoder"}},
		{"workbuddy,workbuddy", []string{"workbuddy"}},
		{"workbuddy,,qoder,", []string{"workbuddy", "qoder"}},
	}
	for _, tc := range cases {
		got := ParseDonationAllowedProviders(tc.in)
		if len(got) != len(tc.want) {
			t.Fatalf("ParseDonationAllowedProviders(%q) = %v, want %v", tc.in, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("ParseDonationAllowedProviders(%q) = %v, want %v", tc.in, got, tc.want)
			}
		}
	}
}
