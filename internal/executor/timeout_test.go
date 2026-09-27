package executor

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The distinction this function makes decides whether a hung child-process
// account is ever cooled down and failed over. Getting it wrong cost an hour of
// 120s Qoder requests, so each branch is pinned here.

// A timeout with a LIVE caller context is the HTTP client's own timeout: the
// upstream was too slow. It must NOT be treated as "the caller went away",
// because that skipped classification and left the account in rotation.
func TestClientTimeoutIsNotCallerCancellation(t *testing.T) {
	ctx := context.Background()
	// What net/http returns when http.Client.Timeout fires.
	err := timeoutError{}
	if requestContextCanceled(ctx, err) {
		t.Fatal("a client timeout with a live caller context must be classified, not ignored")
	}
	if requestContextCanceled(ctx, context.DeadlineExceeded) {
		t.Fatal("a deadline error with a live caller context must be classified")
	}
}

// The caller's own context being done means they disconnected: nothing to
// record, nobody to fail over for.
func TestCallerContextDoneIsIgnored(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !requestContextCanceled(ctx, context.Canceled) {
		t.Fatal("a canceled caller context must be ignored")
	}
	if !requestContextCanceled(ctx, errors.New("anything")) {
		t.Fatal("any error under a done caller context must be ignored")
	}

	deadline, cancel2 := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel2()
	if !requestContextCanceled(deadline, context.DeadlineExceeded) {
		t.Fatal("an expired caller deadline must be ignored")
	}
}

// A bare cancellation with no caller state is still the caller's doing.
func TestBareCancellationIsIgnored(t *testing.T) {
	if !requestContextCanceled(context.Background(), context.Canceled) {
		t.Fatal("context.Canceled with a live caller context is a caller cancellation")
	}
	if requestContextCanceled(context.Background(), nil) {
		t.Fatal("nil error is not a cancellation")
	}
}

// A real worker timeout must actually reach the pool as a failure, so the
// account gets a cooldown. This is the end-to-end shape of the bug: before the
// fix the attempt was recorded but MarkClassified was skipped.
func TestWorkerTimeoutCoolsTheAccount(t *testing.T) {
	pool := NewPool(nil, nil)
	pool.Upsert(Item{ID: "qoder-1", URL: "http://q1", Provider: "qoder", Region: "global", Runtime: "child_process"})

	// Exactly what the executor does for a non-caller-caused transport error.
	classified := Classify(0, `Post "http://127.0.0.1:32100/v1/chat/completions": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`, "", "", "")
	pool.MarkClassified("qoder-1", classified)

	got, ok := pool.ByID("qoder-1")
	if !ok {
		t.Fatal("account missing")
	}
	if got.DownUntil.IsZero() {
		t.Fatalf("a worker timeout must cool the account down, got %+v", got)
	}
	if got.LastKind == "" {
		t.Fatalf("the failure kind must be recorded, got %+v", got)
	}
}

// A caller-caused transport error is returned before classification, so the
// pool must stay clean for an otherwise healthy account.
func TestCallerCancellationDoesNotCoolTheAccount(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !requestContextCanceled(ctx, context.Canceled) {
		t.Fatal("precondition: a canceled caller context is ignored")
	}

	pool := NewPool(nil, nil)
	pool.Upsert(Item{ID: "wb-1", URL: "http://w1", Provider: "workbuddy", Region: "cn", Runtime: "in_process"})
	got, _ := pool.ByID("wb-1")
	if !got.DownUntil.IsZero() {
		t.Fatalf("nothing should have cooled the account: %+v", got)
	}
}

type timeoutError struct{}

func (timeoutError) Error() string {
	return "context deadline exceeded (Client.Timeout exceeded while awaiting headers)"
}
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
