package control

import (
	"context"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// The allow-list must be applied to what the page offers AND to what the
// endpoints accept: hiding a format from the UI is not a policy if a caller can
// post it directly.
//
// The scope is provider+region, not provider: Qoder and WorkBuddy each ship a CN
// and a global deployment, and those are separate accounts. Allowing Qoder CN
// must not also open Qoder Global.
func TestDonationAllowListIsPerProviderRegion(t *testing.T) {
	donations, store, _ := donationHarness(t, &fakeLogin{}, nil)
	settings := NewSettings(store)
	donations.settings = settings
	ctx := context.Background()

	// With nothing stored, every format is offered (historical behaviour).
	all := donations.Formats(ctx)
	if len(all) == 0 {
		t.Fatal("an unset allow-list must not hide every format")
	}

	// Pick a provider that really ships two regions, so the test exercises the
	// distinction rather than a single-region provider.
	var twoRegion *providers.ProviderDescriptor
	for _, descriptor := range providers.List() {
		if len(descriptor.Regions) >= 2 {
			d := descriptor
			twoRegion = &d
			break
		}
	}
	if twoRegion == nil {
		t.Skip("no provider with two regions is registered")
	}
	firstRegion := twoRegion.Regions[0].ID
	secondRegion := twoRegion.Regions[1].ID
	token := donationFormatToken(twoRegion.ID, firstRegion)

	if err := settings.SetSecret(ctx, donationFormatsSecret, token); err != nil {
		t.Fatalf("set allow-list: %v", err)
	}

	filtered := donations.Formats(ctx)
	if len(filtered) == 0 {
		t.Fatalf("allow-list %q produced no formats", token)
	}
	for _, format := range filtered {
		if format.Provider != twoRegion.ID || format.Region != firstRegion {
			t.Fatalf("format %s/%s survived an allow-list of %q", format.Provider, format.Region, token)
		}
	}

	if !donations.DonationAllowed(ctx, twoRegion.ID, firstRegion) {
		t.Fatalf("%s must be contributable", token)
	}
	// The other region of the SAME provider must be refused.
	if donations.DonationAllowed(ctx, twoRegion.ID, secondRegion) {
		t.Fatalf("%s:%s must be refused while the allow-list names only %s", twoRegion.ID, secondRegion, token)
	}
	// And every other provider entirely.
	for _, descriptor := range providers.List() {
		if descriptor.ID == twoRegion.ID {
			continue
		}
		for _, region := range descriptor.Regions {
			if donations.DonationAllowed(ctx, descriptor.ID, region.ID) {
				t.Fatalf("%s:%s must be refused while the allow-list names only %s", descriptor.ID, region.ID, token)
			}
		}
	}

	// Clearing the setting restores "everything".
	if err := settings.SetSecretOrEmpty(ctx, donationFormatsSecret, ""); err != nil {
		t.Fatalf("clear allow-list: %v", err)
	}
	if len(donations.Formats(ctx)) != len(all) {
		t.Fatal("clearing the allow-list must restore every format")
	}
}

// A stale or hand-edited list may contain junk; it must be normalised, and an
// all-empty list must mean "everything" rather than "nothing".
func TestParseDonationAllowedFormats(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{",,", nil},
		{"qoder:cn", []string{"qoder:cn"}},
		{"Qoder:CN", []string{"qoder:cn"}},
		{"qoder:cn,workbuddy:global", []string{"qoder:cn", "workbuddy:global"}},
		{" qoder:cn , workbuddy:global ", []string{"qoder:cn", "workbuddy:global"}},
		{"qoder:cn,qoder:cn", []string{"qoder:cn"}},
		{"qoder:cn,,workbuddy:global,", []string{"qoder:cn", "workbuddy:global"}},
	}
	for _, tc := range cases {
		got := ParseDonationAllowedFormats(tc.in)
		if len(got) != len(tc.want) {
			t.Fatalf("ParseDonationAllowedFormats(%q) = %v, want %v", tc.in, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("ParseDonationAllowedFormats(%q) = %v, want %v", tc.in, got, tc.want)
			}
		}
	}
}
