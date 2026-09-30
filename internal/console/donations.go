package console

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/caigee-cmd/cli2api/internal/control"
)

// donationMaxBody bounds a contribution payload. Credentials are small (a few
// KB at most); the limit keeps an unauthenticated endpoint from buffering an
// unbounded body.
const donationMaxBody = 256 << 10

// HandleDonations serves the donation entry point.
//
//	GET  /api/donations            — accepted formats and how to contribute
//	POST /api/donations            — start a web-authorization round
//	POST /api/donations/credential — submit a pasted credential (legacy)
//
// The endpoint is intentionally public: a contributor has no console key, and
// the reward is credited to their own numeric New API user id. Abuse control is
// therefore the credential itself — an unusable account is rejected by the
// provider importer, or never authorized, before any credit is issued.
func (h *Handler) HandleDonations(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.writeDonationInfo(w, r)
	case http.MethodPost:
		h.HandleDonationStart(w, r)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET or POST only")
	}
}

// HandleDonationStart begins a web-authorized contribution: it creates the
// account, opens the provider's browser login, and returns the URL to visit.
func (h *Handler) HandleDonationStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
		return
	}
	var input control.DonationStart
	if err := decodeDonationBody(r, &input); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if strings.TrimSpace(input.Format) == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request", "format is required")
		return
	}
	donations := h.donations()
	if donations == nil {
		writeErr(w, http.StatusServiceUnavailable, "donations_unavailable", "donations are not available")
		return
	}
	session, err := donations.StartSession(r.Context(), input)
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, session)
}

// HandleDonationCredential keeps the pasted-credential contribution path.
func (h *Handler) HandleDonationCredential(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
		return
	}
	var input control.DonationRequest
	if err := decodeDonationBody(r, &input); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if strings.TrimSpace(input.Format) == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request", "format is required")
		return
	}
	donations := h.donations()
	if donations == nil {
		writeErr(w, http.StatusServiceUnavailable, "donations_unavailable", "donations are not available")
		return
	}
	result, err := donations.Submit(r.Context(), input)
	if err != nil {
		writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

// HandleDonationSession polls, and on completion settles, one contribution.
func (h *Handler) HandleDonationSession(w http.ResponseWriter, r *http.Request) {
	donations := h.donations()
	if donations == nil {
		writeErr(w, http.StatusServiceUnavailable, "donations_unavailable", "donations are not available")
		return
	}
	sessionID := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/donations/sessions/"), "/")
	if sessionID == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request", "session id is required")
		return
	}
	// A trailing /callback finishes the login; anything else is the session id.
	parts := strings.Split(sessionID, "/")
	if len(parts) > 2 || (len(parts) == 2 && parts[1] != "callback" && parts[1] != "restart") {
		writeErr(w, http.StatusNotFound, "not_found", "unknown donation session action")
		return
	}
	id := parts[0]
	isCallback := len(parts) == 2 && parts[1] == "callback"
	isRestart := len(parts) == 2 && parts[1] == "restart"

	if isCallback {
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
			return
		}
		var input struct {
			CallbackURL string `json:"callback_url"`
		}
		if err := decodeDonationBody(r, &input); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		session, err := donations.CompleteSession(r.Context(), id, input.CallbackURL)
		if err != nil {
			writeOperationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, session)
		return
	}

	if isRestart {
		if r.Method != http.MethodPost {
			writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
			return
		}
		session, err := donations.RestartSession(r.Context(), id)
		if err != nil {
			writeOperationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, session)
		return
	}

	switch r.Method {
	case http.MethodGet:
		session, err := donations.PollSession(r.Context(), id)
		if err != nil {
			writeOperationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, session)
	case http.MethodDelete:
		if err := donations.CancelSession(r.Context(), id); err != nil {
			writeOperationError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET or DELETE only")
	}
}

func decodeDonationBody(r *http.Request, target any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, donationMaxBody))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, target)
}

// writeDonationInfo describes what can be contributed so the page can render
// the accepted formats without hardcoding them. Capabilities come from the
// provider adapters, not from a provider list here.
func (h *Handler) writeDonationInfo(w http.ResponseWriter, r *http.Request) {
	donations := h.donations()
	if donations == nil {
		writeErr(w, http.StatusServiceUnavailable, "donations_unavailable", "donations are not available")
		return
	}
	// Report the same reward the payout uses, so the page cannot advertise one
	// amount while the ledger pays another.
	rewardUSD := control.DonationRewardUSD()
	writeJSON(w, http.StatusOK, map[string]any{
		"object":        "donation_info",
		"default_usd":   rewardUSD,
		"quota_per_usd": control.QuotaForUSD(1),
		"formats":       donations.Formats(r.Context()),
	})
}

func (h *Handler) donations() *control.Donations {
	if h == nil || h.Control == nil {
		return nil
	}
	return h.Control.Donations
}
