package control

import (
	"context"
	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/executor"
	"github.com/caigee-cmd/cli2api/internal/providers"
	"github.com/caigee-cmd/cli2api/internal/proxy"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type System struct {
	Settings          *Settings
	Accounts          *Accounts
	Pool              *executor.Pool
	Executor          *executor.ChatExecutor
	CrossProviderPool *atomic.Bool
	Mu                *sync.Mutex
}
type SystemSettingsPatch struct {
	CrossProviderModelPool  *bool             `json:"cross_provider_model_pool"`
	CheckinDisabledAccounts *bool             `json:"checkin_disabled_accounts"`
	RoutingStrategy         *string           `json:"routing_strategy"`
	ProxyURL                *string           `json:"proxy_url"`
	WorkBuddyCheckinTime    *string           `json:"workbuddy_checkin_time"`
	CheckinTimes            map[string]string `json:"checkin_times"`
	DonationBaseURL         *string           `json:"donation_base_url"`
	DonationToken           *string           `json:"donation_token"`
	DonationAllowedFormats  *[]string         `json:"donation_allowed_formats"`
}
type SystemSettings struct {
	CrossProviderModelPool  bool                          `json:"cross_provider_model_pool"`
	CheckinDisabledAccounts bool                          `json:"checkin_disabled_accounts"`
	RoutingStrategy         string                        `json:"routing_strategy"`
	ProxyURL                string                        `json:"proxy_url"`
	WorkBuddyCheckinTime    string                        `json:"workbuddy_checkin_time"`
	CheckinTimes            map[string]string             `json:"checkin_times"`
	Timezone                string                        `json:"timezone"`
	SessionAffinity         executor.SessionAffinityStats `json:"session_affinity"`
	DonationBaseURL         string                        `json:"donation_base_url"`
	DonationConfigured      bool                          `json:"donation_configured"`
	// "provider:region" entries; empty means every account type is open.
	DonationAllowedFormats []string `json:"donation_allowed_formats"`
}

func (h *System) Current(ctx context.Context) SystemSettings {
	var proxyURL string
	var donationBase, donationToken string
	var donationAllowed []string
	checkin := ""
	checkinDisabledAccounts := false
	if h.Settings != nil {
		proxyURL, _, _ = h.Settings.GetSecret(ctx, proxyURLSecret)
		checkin = h.Settings.WorkBuddyCheckinTimeDefault(ctx)
		value, ok, err := h.Settings.GetSecret(ctx, checkinDisabledAccountsSecret)
		if ok && err == nil {
			checkinDisabledAccounts, _ = parseSettingBool(value)
		}
		donationBase, _, _ = h.Settings.GetSecret(ctx, donationBaseURLSecret)
		donationToken, _, _ = h.Settings.GetSecret(ctx, donationTokenSecret)
		donationAllowed = DonationAllowedFormats(ctx, h.Settings)
	}
	settings := SystemSettings{
		CrossProviderModelPool:  h.CrossProviderPool.Load(),
		CheckinDisabledAccounts: checkinDisabledAccounts,
		ProxyURL:                proxy.Redact(proxyURL),
		WorkBuddyCheckinTime:    checkin,
		CheckinTimes:            map[string]string{},
		// The donation token is a credential: report only whether one is set.
		DonationBaseURL:    donationBase,
		DonationConfigured: strings.TrimSpace(donationBase) != "" && strings.TrimSpace(donationToken) != "",
		// An empty list means every provider is contributable, which is what the
		// console shows as "all".
		DonationAllowedFormats: donationAllowed,
		Timezone:               time.Now().Format("MST -07:00"),
	}
	for _, descriptor := range providers.List() {
		if descriptor.SupportsCheckin() && h.Settings != nil {
			settings.CheckinTimes[descriptor.ID], _ = accounts.CheckinTimeDefault(ctx, h.Settings, descriptor.ID)
		}
	}
	if h.Pool != nil {
		settings.RoutingStrategy = h.Pool.RoutingStrategy()
	}
	if h.Executor != nil && h.Executor.SessionAffinity != nil {
		settings.SessionAffinity = h.Executor.SessionAffinity.Stats()
	}
	return settings
}

// descriptorHasRegion reports whether a provider declares the given region. A
// provider with no declared regions accepts only its default one.
func descriptorHasRegion(descriptor providers.ProviderDescriptor, regionID string) bool {
	if len(descriptor.Regions) == 0 {
		return regionID == strings.ToLower(strings.TrimSpace(descriptor.DefaultRegion))
	}
	for _, region := range descriptor.Regions {
		id := strings.ToLower(strings.TrimSpace(region.ID))
		if id == "" {
			id = strings.ToLower(strings.TrimSpace(descriptor.DefaultRegion))
		}
		if id == regionID {
			return true
		}
	}
	return false
}

func (h *System) Patch(ctx context.Context, input SystemSettingsPatch) error {
	if input.CrossProviderModelPool == nil && input.CheckinDisabledAccounts == nil && input.RoutingStrategy == nil && input.ProxyURL == nil && input.WorkBuddyCheckinTime == nil && len(input.CheckinTimes) == 0 && input.DonationBaseURL == nil && input.DonationToken == nil && input.DonationAllowedFormats == nil {
		return operationError("invalid_request", "a system setting is required")
	}
	var strategy string
	if input.RoutingStrategy != nil {
		rawStrategy := strings.ToLower(strings.TrimSpace(*input.RoutingStrategy))
		if rawStrategy != accounts.RoutingStrategyRoundRobin && rawStrategy != accounts.RoutingStrategyWeightedRoundRobin && rawStrategy != accounts.RoutingStrategyFillFirst {
			return operationError("invalid_routing_strategy", "routing_strategy must be round-robin, weighted-round-robin, or fill-first")
		}
		strategy = accounts.NormalizeRoutingStrategy(rawStrategy)
	}
	var checkinTime string
	checkinTimes := make(map[string]string, len(input.CheckinTimes))
	for providerID, value := range input.CheckinTimes {
		descriptor, found := providers.Get(providerID)
		if !found || descriptor.ID != providerID || !descriptor.SupportsCheckin() {
			return operationError("provider_unsupported", "check-in is not available for this provider")
		}
		normalized, err := accounts.NormalizeCheckinTime(value)
		if err != nil {
			return operationError("invalid_checkin_time", err.Error())
		}
		checkinTimes[providerID] = normalized
	}
	if input.WorkBuddyCheckinTime != nil {
		normalized, err := accounts.NormalizeWorkBuddyCheckinTime(*input.WorkBuddyCheckinTime)
		if err != nil || strings.TrimSpace(*input.WorkBuddyCheckinTime) == "" {
			return operationError("invalid_workbuddy_checkin_time", "workbuddy_checkin_time must use HH:mm")
		}
		checkinTime = normalized
		if value, exists := checkinTimes["workbuddy"]; exists && value != checkinTime {
			return operationError("invalid_checkin_time", "conflicting WorkBuddy check-in times")
		}
		checkinTimes["workbuddy"] = checkinTime
	}

	if h.Mu != nil {
		h.Mu.Lock()
		defer h.Mu.Unlock()
	}
	if input.ProxyURL != nil {
		// Read the persisted value, resolve redacted re-submits, validate,
		// and compare inside the same critical section that saves and
		// reloads. Doing the read outside the lock would let two concurrent
		// PATCHes interleave: a request submitting the old value could
		// compute proxyChanged against a stale read and then skip the write
		// while switching the runtime back to the old proxy, leaving the
		// database and the running workers disagreeing.
		existing, _, err := h.Settings.GetSecret(ctx, proxyURLSecret)
		if err != nil {
			return operationError("system_settings_read_failed", err.Error())
		}
		proxyURL := proxy.Preserve(existing, *input.ProxyURL)
		if err := proxy.ValidateHTTPOnly(proxyURL); err != nil {
			return operationError("invalid_proxy_url", err.Error())
		}
		proxyChanged := proxyURL != strings.TrimSpace(existing)

		// Persist clears as an explicit empty value (not a delete) so the
		// next boot distinguishes "user cleared it" from "never set" and
		// does not re-apply the environment bootstrap. An unchanged value
		// skips the write, but we still call ReloadProxyURL: whether the
		// workers actually need restarting is the Manager's call, which
		// knows if a previous reload failed.
		if proxyChanged {
			if err := h.Settings.SetSecretOrEmpty(ctx, proxyURLSecret, proxyURL); err != nil {
				return operationError("system_settings_save_failed", err.Error())
			}
		}
		if err := h.Accounts.ReloadProxyURL(ctx, proxyURL); err != nil {
			return operationError("proxy_reload_failed", err.Error())
		}
	}
	if input.CrossProviderModelPool != nil {
		enabled := *input.CrossProviderModelPool
		value := "0"
		if enabled {
			value = "1"
		}
		if err := h.Settings.SetSecret(ctx, crossProviderModelPoolSecret, value); err != nil {
			return operationError("system_settings_save_failed", err.Error())
		}
		h.CrossProviderPool.Store(enabled)
	}
	if input.CheckinDisabledAccounts != nil {
		value := "0"
		if *input.CheckinDisabledAccounts {
			value = "1"
		}
		if err := h.Settings.SetSecret(ctx, checkinDisabledAccountsSecret, value); err != nil {
			return operationError("system_settings_save_failed", err.Error())
		}
	}
	if input.RoutingStrategy != nil {
		if err := h.Settings.SetSecret(ctx, routingStrategySecret, strategy); err != nil {
			return operationError("system_settings_save_failed", err.Error())
		}
		h.Pool.SetRoutingStrategy(strategy)
	}
	for providerID, value := range checkinTimes {
		if err := h.Settings.SetSecret(ctx, accounts.CheckinTimeSecret(providerID), value); err != nil {
			return operationError("system_settings_save_failed", err.Error())
		}
	}
	if input.DonationBaseURL != nil {
		base := strings.TrimRight(strings.TrimSpace(*input.DonationBaseURL), "/")
		if base != "" && !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
			return operationError("invalid_request", "donation_base_url must be an http(s) URL")
		}
		if err := h.Settings.SetSecret(ctx, donationBaseURLSecret, base); err != nil {
			return operationError("system_settings_save_failed", err.Error())
		}
	}
	if input.DonationToken != nil {
		if err := h.Settings.SetSecret(ctx, donationTokenSecret, strings.TrimSpace(*input.DonationToken)); err != nil {
			return operationError("system_settings_save_failed", err.Error())
		}
	}
	if input.DonationAllowedFormats != nil {
		// Validate against the provider registry rather than trusting the list:
		// an unknown entry would silently match nothing and read as "no
		// contributions accepted", which is not what the operator asked for.
		// Each entry is "provider:region", and the region must exist on that
		// provider -- allowing "qoder:global" must not also open "qoder:cn".
		normalized := make([]string, 0, len(*input.DonationAllowedFormats))
		seen := map[string]struct{}{}
		for _, raw := range *input.DonationAllowedFormats {
			entry := strings.ToLower(strings.TrimSpace(raw))
			if entry == "" {
				continue
			}
			providerID, regionID, hasRegion := strings.Cut(entry, ":")
			if !hasRegion || regionID == "" {
				return operationError("provider_unsupported", "donation_allowed_formats entries must be provider:region, got "+entry)
			}
			descriptor, found := providers.Get(providerID)
			if !found || descriptor.ID != providerID {
				return operationError("provider_unsupported", "unknown provider in donation_allowed_formats: "+providerID)
			}
			if !descriptorHasRegion(descriptor, regionID) {
				return operationError("provider_unsupported", "unknown region in donation_allowed_formats: "+entry)
			}
			if _, dup := seen[entry]; dup {
				continue
			}
			seen[entry] = struct{}{}
			normalized = append(normalized, entry)
		}
		if err := h.Settings.SetSecretOrEmpty(ctx, donationFormatsSecret, strings.Join(normalized, ",")); err != nil {
			return operationError("system_settings_save_failed", err.Error())
		}
	}
	return nil
}
