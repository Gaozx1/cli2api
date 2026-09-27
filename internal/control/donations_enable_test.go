package control

import (
	"context"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/providers"
)

// The web-authorization path must create its account enabled.
//
// A Qoder login runs inside the account's child worker, and the worker only
// starts for an enabled account, so a disabled placeholder makes StartLogin fail
// with "account is disabled or not running" and the contribution cannot even
// begin. This is safe because the account has no credential until the provider
// authorizes it, and an account that is not ready is never routed to.
func TestWebAuthSessionCreatesEnabledAccount(t *testing.T) {
	login := &fakeLogin{authURL: "https://provider.example/auth"}
	donations, store, _ := donationHarness(t, login, nil)

	session, err := donations.StartSession(context.Background(), DonationStart{
		Format: "workbuddy-oauth-v1", NewAPIUserID: 7,
	})
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if !store.accounts[session.AccountID].Enabled {
		t.Fatal("the web-auth placeholder must be enabled so the provider worker can start")
	}
}

// A Qoder round must be able to start at all. Qoder's login goes through its
// child worker, so this is the case a disabled placeholder breaks outright.
func TestQoderWebAuthSessionStarts(t *testing.T) {
	login := &fakeLogin{authURL: "https://qoder.example/device"}
	donations, store, _ := donationHarness(t, login, nil)
	donations.accounts.Providers.Register(providers.Adapter{ID: "qoder", Login: login})

	session, err := donations.StartSession(context.Background(), DonationStart{
		Format: "qoder-native-v1", NewAPIUserID: 7,
	})
	if err != nil {
		t.Fatalf("a qoder contribution must be able to start: %v", err)
	}
	if session.AuthURL == "" {
		t.Fatal("a qoder round must return an authorization URL")
	}
	if !store.accounts[session.AccountID].Enabled {
		t.Fatal("the qoder placeholder must be enabled for its worker to run")
	}
}

// The pasted-credential path's disabled import and the host strip are covered by
// donations_hardening_test.go (TestSubmitImportsContributedAccountDisabled,
// TestStripDonationCredentialHostsRemovesUpstreamOverrides). What is asserted
// here is the other half: the web-auth path must stay enabled, because that is
// what makes a Qoder contribution startable at all.
