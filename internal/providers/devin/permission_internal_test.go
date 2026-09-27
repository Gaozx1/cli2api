package devin

import (
	"net/http"
	"testing"

	"github.com/caigee-cmd/cli2api/internal/accounts"
)

// Devin reports some of its OWN faults under permission_denied, e.g.
// "permission_denied: an internal error occurred (trace ID: ...)". The code says
// "the caller may not do this", the message says "the server failed".
//
// Mapping that to 403 made Classify return invalid_request, which neither fails
// over nor cools the account -- so every request bounced off the same broken
// upstream forever. Measured live: all three Devin accounts failed every chat
// attempt this way while GetUserStatus still returned a valid session and 100%
// quota.
func TestPermissionDeniedWithInternalErrorIsUpstream(t *testing.T) {
	payload := []byte(`{"error":{"code":"permission_denied","message":"an internal error occurred (trace ID: 7b8e625ccf9bca9c6c559c6b990fc811)"}}`)
	status, err := ParseTrailerError(payload)
	if err == nil {
		t.Fatal("a trailer error must produce an error")
	}
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: an upstream internal error must not read as a request rejection", status)
	}

	classified := Classify(status, err.Error())
	if classified.Kind != accounts.KindUnavailable {
		t.Fatalf("kind = %s, want unavailable", classified.Kind)
	}
}

// A genuine permission_denied -- the caller's request shape -- keeps the old
// behaviour: 403, invalid_request, no cooldown.
func TestPlainPermissionDeniedStaysRequestLevel(t *testing.T) {
	payload := []byte(`{"error":{"code":"permission_denied","message":"this account may not use that tool"}}`)
	status, err := ParseTrailerError(payload)
	if err == nil {
		t.Fatal("a trailer error must produce an error")
	}
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	classified := Classify(status, err.Error())
	if classified.Kind != accounts.KindInvalidRequest {
		t.Fatalf("kind = %s, want invalid_request", classified.Kind)
	}
	if classified.Status != 400 && classified.Status != 403 {
		t.Fatalf("status = %d, want 400 or 403", classified.Status)
	}
}

// The MCP-configuration denial is a request-shape problem and must stay that way,
// even though it also carries permission_denied.
func TestMCPDenialStillRequestLevel(t *testing.T) {
	payload := []byte(`{"error":{"code":"permission_denied","message":"Unable to process request due to an MCP configuration issue."}}`)
	status, err := ParseTrailerError(payload)
	if err == nil {
		t.Fatal("a trailer error must produce an error")
	}
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (MCP denial)", status)
	}
	classified := Classify(status, err.Error())
	if classified.Kind != accounts.KindInvalidRequest {
		t.Fatalf("kind = %s, want invalid_request", classified.Kind)
	}
}

// invalid_argument keeps its existing split, now via the shared helper.
func TestInvalidArgumentInternalErrorStaysUpstream(t *testing.T) {
	payload := []byte(`{"error":{"code":"invalid_argument","message":"an internal error occurred (trace ID: abc)"}}`)
	status, _ := ParseTrailerError(payload)
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", status)
	}
	payload = []byte(`{"error":{"code":"invalid_argument","message":"bad field: model"}}`)
	status, _ = ParseTrailerError(payload)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
}
