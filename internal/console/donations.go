package console

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/caigee-cmd/cli2api/internal/control"
	"github.com/caigee-cmd/cli2api/internal/providers"
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
		h.writeDonationInfo(w)
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
	if sessionID == "" || strings.Contains(sessionID, "/") {
		writeErr(w, http.StatusBadRequest, "invalid_request", "session id is required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		session, err := donations.PollSession(r.Context(), sessionID)
		if err != nil {
			writeOperationError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, session)
	case http.MethodDelete:
		if err := donations.CancelSession(r.Context(), sessionID); err != nil {
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
// the accepted formats without hardcoding them.
func (h *Handler) writeDonationInfo(w http.ResponseWriter) {
	type donationFormat struct {
		Format     string `json:"format"`
		Provider   string `json:"provider"`
		Label      string `json:"label"`
		Region     string `json:"region"`
		Credential string `json:"credential_kind"`
		// WebAuth is true when the account can be authorized through the
		// provider's own browser login, which is the preferred flow.
		WebAuth     bool   `json:"web_auth"`
		Description string `json:"description"`
	}
	formats := make([]donationFormat, 0, 2)
	for _, descriptor := range providers.List() {
		for _, format := range descriptor.CredentialFormats {
			kind := "json"
			if format == donationQoderFormat {
				kind = "qoder_native"
			}
			formats = append(formats, donationFormat{
				Format:      format,
				Provider:    descriptor.ID,
				Label:       descriptor.Label,
				Region:      descriptor.DefaultRegion,
				Credential:  kind,
				WebAuth:     descriptor.Capabilities.BrowserLogin,
				Description: descriptor.Label + " account contribution",
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object":        "donation_info",
		"default_usd":   1,
		"quota_per_usd": control.QuotaForUSD(1),
		"formats":       formats,
	})
}

func (h *Handler) donations() *control.Donations {
	if h == nil || h.Control == nil {
		return nil
	}
	return h.Control.Donations
}

// donationQoderFormat mirrors control's constant; the console only needs it to
// label the credential kind in the info payload.
const donationQoderFormat = "qoder-native-v1"
