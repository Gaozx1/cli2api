package qoder

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/accounts"
)

type recordingStore struct {
	saved    []accounts.NativeCredential
	authType string
	failWith error
}

func (s *recordingStore) LoadCredential(context.Context, string) (accounts.NativeCredential, error) {
	return accounts.NativeCredential{}, errors.New("not used")
}

func (s *recordingStore) SaveCredential(_ context.Context, _ string, authType string, credential accounts.NativeCredential) error {
	if s.failWith != nil {
		return s.failWith
	}
	s.authType = authType
	s.saved = append(s.saved, credential)
	return nil
}

func writeAuth(t *testing.T, home, region, user, machine string) {
	t.Helper()
	dir := AuthDir(home, region)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if user != "" {
		if err := os.WriteFile(filepath.Join(dir, "user"), []byte(user), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if machine != "" {
		if err := os.WriteFile(filepath.Join(dir, "machine_id"), []byte(machine), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func account() accounts.Account {
	return accounts.Account{ID: "acc1", Provider: "qoder", ProviderRegion: "global"}
}

// A live login in the worker home is persisted, so a tmpfs wipe cannot lose it.
// This is the contributed-account bug: it served chat until a restart because
// nothing had ever written the credential to the store.
func TestSyncIfPresentPersistsLiveLogin(t *testing.T) {
	home := t.TempDir()
	writeAuth(t, home, "global", "user-blob-bytes", "machine-1")
	store := &recordingStore{}

	saved, err := SyncCredentialIfPresent(context.Background(), store, account(), home, "oauth")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !saved {
		t.Fatal("a present login must be saved")
	}
	if len(store.saved) != 1 {
		t.Fatalf("saved %d credentials, want 1", len(store.saved))
	}
	got := store.saved[0]
	if string(got.UserBlob) != "user-blob-bytes" || got.MachineID != "machine-1" {
		t.Fatalf("saved credential = %+v", got)
	}
	if store.authType != "oauth" {
		t.Fatalf("authType = %q, want oauth", store.authType)
	}
}

// An account that has not authorized yet has nothing to save, and that is not
// an error: the caller must not fail a contribution over it.
func TestSyncIfPresentIsQuietWhenNothingToSave(t *testing.T) {
	home := t.TempDir()
	writeAuth(t, home, "global", "", "machine-1") // machine id only, no user blob
	store := &recordingStore{}

	saved, err := SyncCredentialIfPresent(context.Background(), store, account(), home, "oauth")
	if err != nil {
		t.Fatalf("a missing login must not be an error: %v", err)
	}
	if saved || len(store.saved) != 0 {
		t.Fatal("nothing should have been saved")
	}

	// A completely absent home is the same story.
	saved, err = SyncCredentialIfPresent(context.Background(), store, account(), t.TempDir(), "oauth")
	if err != nil || saved {
		t.Fatalf("absent home: saved=%v err=%v", saved, err)
	}
}

// A blank user blob or machine id must not be stored: SaveCredential rejects it,
// and a half-written credential is worse than none.
func TestSyncIfPresentRejectsEmptyBlob(t *testing.T) {
	store := &recordingStore{}

	// User blob present, machine id blank.
	blankMachine := t.TempDir()
	if err := os.MkdirAll(AuthDir(blankMachine, "global"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(AuthDir(blankMachine, "global"), "user"), []byte("blob"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(AuthDir(blankMachine, "global"), "machine_id"), []byte("   "), 0o600); err != nil {
		t.Fatal(err)
	}
	if saved, err := SyncCredentialIfPresent(context.Background(), store, account(), blankMachine, "oauth"); err != nil || saved {
		t.Fatalf("blank machine id: saved=%v err=%v", saved, err)
	}
	if len(store.saved) != 0 {
		t.Fatal("a blank machine id must not be saved")
	}

	// Machine id present, user blob empty.
	blankUser := t.TempDir()
	if err := os.MkdirAll(AuthDir(blankUser, "global"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(AuthDir(blankUser, "global"), "user"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(AuthDir(blankUser, "global"), "machine_id"), []byte("m"), 0o600); err != nil {
		t.Fatal(err)
	}
	if saved, err := SyncCredentialIfPresent(context.Background(), store, account(), blankUser, "oauth"); err != nil || saved {
		t.Fatalf("empty user blob: saved=%v err=%v", saved, err)
	}
	if len(store.saved) != 0 {
		t.Fatal("an empty user blob must not be saved")
	}
}

// A store failure surfaces so the caller can log it, but the live login is not
// silently reported as saved.
func TestSyncIfPresentReportsStoreFailure(t *testing.T) {
	home := t.TempDir()
	writeAuth(t, home, "global", "blob", "machine-1")
	store := &recordingStore{failWith: errors.New("db down")}
	saved, err := SyncCredentialIfPresent(context.Background(), store, account(), home, "oauth")
	if err == nil {
		t.Fatal("a store failure must surface")
	}
	if saved {
		t.Fatal("a failed save must not report success")
	}
}

// The region picks the config dir, so a CN account reads its own home.
func TestSyncIfPresentUsesRegionHome(t *testing.T) {
	home := t.TempDir()
	writeAuth(t, home, "cn", "cn-blob", "cn-machine")
	store := &recordingStore{}
	cn := accounts.Account{ID: "acc-cn", Provider: "qoder", ProviderRegion: "cn"}

	saved, err := SyncCredentialIfPresent(context.Background(), store, cn, home, "oauth")
	if err != nil || !saved {
		t.Fatalf("saved=%v err=%v", saved, err)
	}
	if string(store.saved[0].UserBlob) != "cn-blob" {
		t.Fatalf("read the wrong home: %+v", store.saved[0])
	}
}
