package qoder

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/caigee-cmd/cli2api/internal/accounts"
)

const CredentialFormat = "qoder-native-v1"

type CredentialStore interface {
	LoadCredential(ctx context.Context, accountID string) (accounts.NativeCredential, error)
	SaveCredential(ctx context.Context, accountID, authType string, credential accounts.NativeCredential) error
}

func ConfigDirName(region string) string {
	if strings.EqualFold(strings.TrimSpace(region), "cn") {
		return ".qoder-cn"
	}
	return ".qoder"
}

func AuthDir(home, region string) string {
	return filepath.Join(home, ConfigDirName(region), ".auth")
}

func MaterializeHome(ctx context.Context, store CredentialStore, account accounts.Account, home string) error {
	authDir := AuthDir(home, account.ProviderRegion)
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		return fmt.Errorf("create account home: %w", err)
	}
	credential, err := store.LoadCredential(ctx, account.ID)
	if errors.Is(err, accounts.ErrAccountNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(authDir, "user"), credential.UserBlob, 0o600); err != nil {
		return fmt.Errorf("write user credential: %w", err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "machine_id"), []byte(credential.MachineID), 0o600); err != nil {
		return fmt.Errorf("write machine id: %w", err)
	}
	return nil
}

func SyncCredential(ctx context.Context, store CredentialStore, account accounts.Account, home, authType string) error {
	authDir := AuthDir(home, account.ProviderRegion)
	userBlob, err := os.ReadFile(filepath.Join(authDir, "user"))
	if err != nil {
		return fmt.Errorf("read qoder user credential: %w", err)
	}
	machineID, err := os.ReadFile(filepath.Join(authDir, "machine_id"))
	if err != nil {
		return fmt.Errorf("read qoder machine id: %w", err)
	}
	return store.SaveCredential(ctx, account.ID, authType, accounts.NativeCredential{
		UserBlob:  userBlob,
		MachineID: string(machineID),
	})
}

// SyncCredentialIfPresent stores the login the worker already wrote into its
// home, and reports whether there was one to store.
//
// The worker home lives on tmpfs and is wiped on every restart, so a login that
// only exists there is lost the moment the process restarts. MaterializeHome
// restores a login from the store on startup; without a stored credential there
// is nothing to restore, and the account comes back as "needs login" even though
// it had been serving. That is exactly what happened to a contributed Qoder
// account: it was authorized through the donations flow, served chat, and then
// silently lost its login on the next restart because nothing had persisted it.
//
// Callers use this right after an authorization completes to make the login
// survive. Absent files are not an error: an account that has not authorized yet
// simply has nothing to save.
func SyncCredentialIfPresent(ctx context.Context, store CredentialStore, account accounts.Account, home, authType string) (bool, error) {
	authDir := AuthDir(home, account.ProviderRegion)
	userBlob, err := os.ReadFile(filepath.Join(authDir, "user"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read qoder user credential: %w", err)
	}
	machineID, err := os.ReadFile(filepath.Join(authDir, "machine_id"))
	if err != nil {
		return false, fmt.Errorf("read qoder machine id: %w", err)
	}
	if len(userBlob) == 0 || strings.TrimSpace(string(machineID)) == "" {
		return false, nil
	}
	if err := store.SaveCredential(ctx, account.ID, authType, accounts.NativeCredential{
		UserBlob:  userBlob,
		MachineID: string(machineID),
	}); err != nil {
		return false, err
	}
	return true, nil
}

type RuntimePaths struct {
	CLIPath   string
	Site      string
	ConfigDir string
	ConfigEnv string
}

func RuntimeSpec(cliPath, cnCLIPath, region, home string) (RuntimePaths, error) {
	region = strings.ToLower(strings.TrimSpace(region))
	configDir := filepath.Join(home, ConfigDirName(region))
	switch region {
	case "", "global":
		cliPath = strings.TrimSpace(cliPath)
		if cliPath == "" {
			return RuntimePaths{}, fmt.Errorf("qoder global CLI path required")
		}
		return RuntimePaths{CLIPath: cliPath, Site: "global", ConfigDir: configDir, ConfigEnv: "QODER_CONFIG_DIR"}, nil
	case "cn":
		cnCLIPath = strings.TrimSpace(cnCLIPath)
		if cnCLIPath == "" {
			return RuntimePaths{}, fmt.Errorf("qoder CN CLI path required: set QODERCNCLI_JS to @qodercn-ai/qoderclicn bundle/qoderclicn.js")
		}
		return RuntimePaths{CLIPath: cnCLIPath, Site: "cn", ConfigDir: configDir, ConfigEnv: "QODERCN_CONFIG_DIR"}, nil
	default:
		return RuntimePaths{}, fmt.Errorf("unknown qoder region %q", region)
	}
}
