package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// retryableTestError is a RetryableError reporting Retryable() true.
type retryableTestError struct{ msg string }

func (e retryableTestError) Error() string   { return e.msg }
func (e retryableTestError) Retryable() bool { return true }

// nonRetryableTestError implements RetryableError but reports false, so it
// must behave exactly like a plain error.
type nonRetryableTestError struct{ msg string }

func (e nonRetryableTestError) Error() string   { return e.msg }
func (e nonRetryableTestError) Retryable() bool { return false }

// flakyProvider returns scripted errors per Apply call: errs[i] is returned
// on the i-th call, nil once the script is exhausted. Every request is
// recorded in order.
type flakyProvider struct {
	errs  []error
	calls []ChangeRequest
}

func (p *flakyProvider) Apply(req ChangeRequest) error {
	idx := len(p.calls)
	p.calls = append(p.calls, req)
	if idx < len(p.errs) {
		return p.errs[idx]
	}
	return nil
}

func TestApplyRetriesRetryableErrorUntilSuccess(t *testing.T) {
	body := planBody(`[`+resEntry("a", `[]`)+`,`+resEntry("b", `[]`)+`]`, `[]`)
	provider := &flakyProvider{errs: []error{
		retryableTestError{"transient 1"},
		retryableTestError{"transient 2"},
	}}
	res := doApply(t, HandlerWithProvider(provider), body, nil)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
	if len(provider.calls) != 4 {
		t.Fatalf("calls = %v, want 4 (a retried twice, then b)", providerCallsSummary(provider.calls))
	}
	// Attempts of a are 1,2,3; b is attempted once at 1.
	for i, want := range []int{1, 2, 3, 1} {
		if provider.calls[i].Attempt != want {
			t.Fatalf("call %d attempt = %d, want %d", i, provider.calls[i].Attempt, want)
		}
	}
	// The retried change keeps one key; the other change gets a different
	// non-empty key.
	keyA := provider.calls[0].IdempotencyKey
	if keyA == "" {
		t.Fatal("IdempotencyKey must be non-empty")
	}
	for i := 1; i <= 2; i++ {
		if provider.calls[i].IdempotencyKey != keyA {
			t.Fatalf("call %d key = %q, want the same key %q across attempts", i, provider.calls[i].IdempotencyKey, keyA)
		}
	}
	if provider.calls[3].IdempotencyKey == "" || provider.calls[3].IdempotencyKey == keyA {
		t.Fatalf("distinct changes must get distinct non-empty keys: %q vs %q", keyA, provider.calls[3].IdempotencyKey)
	}
	// The retried change is recorded exactly once, in plan order.
	if res.Applied == nil || len(*res.Applied) != 2 {
		t.Fatalf("applied = %v, want 2 entries", res.Applied)
	}
	if (*res.Applied)[0].Address != "a" || (*res.Applied)[1].Address != "b" {
		t.Fatalf("applied order = %v", *res.Applied)
	}
}

func TestApplyRetryableExhaustsThreeAttempts(t *testing.T) {
	body := planBody(`[`+resEntry("a", `[]`)+`]`, `[]`)
	provider := &flakyProvider{errs: []error{
		retryableTestError{"fail 1"},
		retryableTestError{"fail 2"},
		retryableTestError{"fail 3"},
	}}
	res := doApply(t, HandlerWithProvider(provider), body, nil)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if len(provider.calls) != 3 {
		t.Fatalf("calls = %d, want exactly 3 attempts", len(provider.calls))
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeProviderError {
		t.Fatalf("errors = %+v", res.Errors)
	}
	if res.Errors[0].Message != "fail 3" {
		t.Fatalf("message = %q, want the last attempt's text", res.Errors[0].Message)
	}
	if res.Errors[0].Path != "/changes/0" {
		t.Fatalf("path = %q, want /changes/0", res.Errors[0].Path)
	}
	if res.Applied == nil || len(*res.Applied) != 0 {
		t.Fatalf("applied = %v, want empty", res.Applied)
	}
}

func TestApplyRetryableThenPlainErrorStops(t *testing.T) {
	body := planBody(`[`+resEntry("a", `[]`)+`]`, `[]`)
	provider := &flakyProvider{errs: []error{
		retryableTestError{"transient"},
		errors.New("permanent"),
	}}
	res := doApply(t, HandlerWithProvider(provider), body, nil)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if len(provider.calls) != 2 {
		t.Fatalf("calls = %d, want 2 (retry stops on a plain error)", len(provider.calls))
	}
	if len(res.Errors) != 1 || res.Errors[0].Message != "permanent" {
		t.Fatalf("errors = %+v, want the plain error's text", res.Errors)
	}
}

func TestApplyRetryableFalseIsNotRetried(t *testing.T) {
	body := planBody(`[`+resEntry("a", `[]`)+`]`, `[]`)
	provider := &flakyProvider{errs: []error{nonRetryableTestError{"no retry"}}}
	res := doApply(t, HandlerWithProvider(provider), body, nil)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if len(provider.calls) != 1 {
		t.Fatalf("calls = %d, want 1 (Retryable() false must not retry)", len(provider.calls))
	}
	if len(res.Errors) != 1 || res.Errors[0].Message != "no retry" {
		t.Fatalf("errors = %+v", res.Errors)
	}
}

func TestApplyFirstAttemptCarriesKeyAndAttemptOne(t *testing.T) {
	body := planBody(`[`+resEntry("a", `[]`)+`]`, `[]`)
	provider := &flakyProvider{}
	res := doApply(t, HandlerWithProvider(provider), body, nil)
	if res.status != http.StatusOK {
		t.Fatalf("status = %d errors=%v", res.status, res.Errors)
	}
	if len(provider.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(provider.calls))
	}
	call := provider.calls[0]
	if call.Attempt != 1 {
		t.Fatalf("attempt = %d, want 1", call.Attempt)
	}
	if call.IdempotencyKey == "" {
		t.Fatal("IdempotencyKey must be non-empty")
	}
}

func TestApplyIdempotencyKeysNotReusedAcrossRequests(t *testing.T) {
	provider := &flakyProvider{}
	handler := HandlerWithProvider(provider)
	body := planBody(`[`+resEntry("a", `[]`)+`]`, `[]`)
	for i := 0; i < 2; i++ {
		if res := doApply(t, handler, body, nil); res.status != http.StatusOK {
			t.Fatalf("request %d status = %d", i, res.status)
		}
	}
	if len(provider.calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(provider.calls))
	}
	first, second := provider.calls[0].IdempotencyKey, provider.calls[1].IdempotencyKey
	if first == "" || second == "" || first == second {
		t.Fatalf("keys of separate requests must differ: %q vs %q", first, second)
	}
}

func TestApplyRetryDoesNotAdvanceLaterChanges(t *testing.T) {
	// a depends on b, so b executes first; b fails retryably once.
	body := planBody(`[`+resEntry("a", `["b"]`)+`,`+resEntry("b", `[]`)+`]`, `[]`)
	provider := &flakyProvider{errs: []error{retryableTestError{"flaky b"}}}
	res := doApply(t, HandlerWithProvider(provider), body, nil)
	if res.status != http.StatusOK {
		t.Fatalf("status = %d errors=%v", res.status, res.Errors)
	}
	if got := providerCallsSummary(provider.calls); got != "create:b|create:b|create:a" {
		t.Fatalf("calls = %q, want b retried in place before a starts", got)
	}
	if provider.calls[0].IdempotencyKey != provider.calls[1].IdempotencyKey {
		t.Fatal("retries of b must share one key")
	}
	if provider.calls[2].IdempotencyKey == provider.calls[0].IdempotencyKey {
		t.Fatal("a must get its own key")
	}
}

func TestApplyRetryAbortsWhenRequestContextDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	// Cancel the request context from inside the first attempt; the retry
	// must not call the Provider again.
	handler := HandlerWithProvider(providerFunc(func(req ChangeRequest) error {
		calls++
		cancel()
		return retryableTestError{"transient"}
	}))
	body := planBody(`[`+resEntry("a", `[]`)+`]`, `[]`)
	res := doApply(t, handler, body, ctx)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if calls != 1 {
		t.Fatalf("provider called %d times, want exactly 1 after context cancellation", calls)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeProviderError {
		t.Fatalf("errors = %+v", res.Errors)
	}
	if res.Errors[0].Message != context.Canceled.Error() {
		t.Fatalf("message = %q, want %q", res.Errors[0].Message, context.Canceled.Error())
	}
}

// providerFunc adapts a function to the Provider interface for tests that
// need behavior beyond a scripted error list.
type providerFunc func(ChangeRequest) error

func (f providerFunc) Apply(req ChangeRequest) error { return f(req) }

func TestStateApplyRetriesAndCommitsOnce(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 5, `[]`)
	provider := &flakyProvider{errs: []error{retryableTestError{"flaky"}}}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doStateApply(t, handler, stateConfigBody(`[`+resEntry("a", `[]`)+`]`))
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("status = %d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
	if len(provider.calls) != 2 || provider.calls[0].Attempt != 1 || provider.calls[1].Attempt != 2 {
		t.Fatalf("calls = %+v, want 2 attempts numbered 1,2", provider.calls)
	}
	if provider.calls[0].IdempotencyKey == "" ||
		provider.calls[0].IdempotencyKey != provider.calls[1].IdempotencyKey {
		t.Fatalf("attempts must share one non-empty key: %q, %q",
			provider.calls[0].IdempotencyKey, provider.calls[1].IdempotencyKey)
	}
	if res.Revision == nil || *res.Revision != 6 {
		t.Fatalf("revision = %v, want exactly one increment to 6", res.Revision)
	}
	if res.Applied == nil || len(*res.Applied) != 1 {
		t.Fatalf("applied = %v, want the change recorded once", res.Applied)
	}
	got := doStateGet(t, handler)
	if got.Revision != 6 || got.State == nil || len(got.State.Resources) != 1 {
		t.Fatalf("stored state = rev %d %+v, want rev 6 with one resource", got.Revision, got.State)
	}
}

func TestStateApplyRetryExhaustedKeepsFirstFailureSemantics(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 2, `[]`)
	provider := &flakyProvider{errs: []error{
		retryableTestError{"e1"},
		retryableTestError{"e2"},
		retryableTestError{"e3"},
	}}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doStateApply(t, handler, stateConfigBody(`[`+resEntry("a", `[]`)+`]`))
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if len(provider.calls) != 3 {
		t.Fatalf("calls = %d, want 3 attempts", len(provider.calls))
	}
	if len(res.Errors) != 1 || res.Errors[0].Message != "e3" || res.Errors[0].Path != "/changes/0" {
		t.Fatalf("errors = %+v, want the last attempt's text at /changes/0", res.Errors)
	}
	// A first-change failure keeps the file and its revision untouched.
	if res.Revision == nil || *res.Revision != 2 {
		t.Fatalf("revision = %v, want unchanged 2", res.Revision)
	}
	got := doStateGet(t, handler)
	if got.Revision != 2 || got.State == nil || len(got.State.Resources) != 0 {
		t.Fatalf("stored state = rev %d %+v, want untouched rev 2 empty", got.Revision, got.State)
	}
}

func TestReconcileRetriesApply(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 4, `[`+stateEntry("a", "t", `{}`, `[]`)+`]`)
	provider := &reconcileProvider{
		snapshots: map[string]*Snapshot{"a": nil}, // missing remotely: recreate
		applyErr:  retryableTestError{"flaky"},
	}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doReconcile(t, handler, `{}`)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("status = %d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
	if len(provider.applies) != 2 {
		t.Fatalf("applies = %d, want 2 attempts", len(provider.applies))
	}
	first, second := provider.applies[0], provider.applies[1]
	if first.Attempt != 1 || second.Attempt != 2 {
		t.Fatalf("attempts = %d,%d, want 1,2", first.Attempt, second.Attempt)
	}
	if first.IdempotencyKey == "" || first.IdempotencyKey != second.IdempotencyKey {
		t.Fatalf("attempts must share one non-empty key: %q, %q", first.IdempotencyKey, second.IdempotencyKey)
	}
	if res.Applied == nil || len(*res.Applied) != 1 {
		t.Fatalf("applied = %v, want the change recorded once", res.Applied)
	}
	if res.Revision == nil || *res.Revision != 4 {
		t.Fatalf("revision = %v, want unchanged 4", res.Revision)
	}
}

func TestReconcileRetryExhaustedReportsLastError(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 1, `[`+stateEntry("a", "t", `{}`, `[]`)+`]`)
	// Fail every attempt with a distinct retryable error.
	scripted := &flakyProvider{errs: []error{
		retryableTestError{"r1"},
		retryableTestError{"r2"},
		retryableTestError{"r3"},
	}}
	handler := HandlerWithProviderAndStateFile(
		observerWrap{scripted, map[string]*Snapshot{"a": nil}}, path)

	res := doReconcile(t, handler, `{}`)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if len(scripted.calls) != 3 {
		t.Fatalf("applies = %d, want 3 attempts", len(scripted.calls))
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeProviderError ||
		res.Errors[0].Message != "r3" || res.Errors[0].Path != "/changes/0" {
		t.Fatalf("errors = %+v, want the last attempt's text at /changes/0", res.Errors)
	}
	if res.Applied == nil || len(*res.Applied) != 0 {
		t.Fatalf("applied = %v, want empty", res.Applied)
	}
}

// observerWrap combines a scripted Apply with static Observe results.
type observerWrap struct {
	*flakyProvider
	snapshots map[string]*Snapshot
}

func (o observerWrap) Observe(ctx context.Context, address string) (*Snapshot, error) {
	return o.snapshots[address], nil
}

func TestMultiProviderRetryUsesDispatchedProvider(t *testing.T) {
	// u1 (type a) fails retryably once on its own Provider; u2 (type b) is
	// unaffected and still runs exactly once, after u's attempts.
	flaky := &flakyProvider{errs: []error{retryableTestError{"flaky a"}}}
	other := &flakyProvider{}
	handler := HandlerWithProviders(map[string]Provider{"a": flaky, "b": other})
	body := multiPlanBody(
		`[`+typedEntry("u1", "a", `[]`)+`,`+typedEntry("u2", "b", `["u1"]`)+`]`, `[]`)
	req := httptest.NewRequest(http.MethodPost, "/v1/apply", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if len(flaky.calls) != 2 || flaky.calls[0].Attempt != 1 || flaky.calls[1].Attempt != 2 {
		t.Fatalf("type-a calls = %+v, want 2 attempts numbered 1,2", flaky.calls)
	}
	if flaky.calls[0].IdempotencyKey == "" ||
		flaky.calls[0].IdempotencyKey != flaky.calls[1].IdempotencyKey {
		t.Fatal("retries of u1 must share one non-empty key")
	}
	if len(other.calls) != 1 || other.calls[0].Attempt != 1 {
		t.Fatalf("type-b calls = %+v, want exactly 1 attempt", other.calls)
	}
	if other.calls[0].IdempotencyKey == flaky.calls[0].IdempotencyKey {
		t.Fatal("distinct changes must get distinct keys")
	}
}

func TestObserverErrorsAreNotRetried(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 1, `[`+stateEntry("a", "t", `{}`, `[]`)+`]`)
	provider := &reconcileProvider{
		observeErrs: map[string]error{"a": retryableTestError{"observe flaky"}},
	}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doReconcile(t, handler, `{}`)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeObserverError {
		t.Fatalf("errors = %+v, want observer_error", res.Errors)
	}
	if len(provider.observes) != 1 {
		t.Fatalf("observes = %d, want exactly 1 (Observer is never retried)", len(provider.observes))
	}
	if len(provider.applies) != 0 {
		t.Fatalf("applies = %d, want none", len(provider.applies))
	}
}

func TestDriftObserveErrorsAreNotRetried(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 1, `[`+stateEntry("a", "t", `{}`, `[]`)+`]`)
	provider := &driftProvider{errs: map[string]error{"a": retryableTestError{"observe flaky"}}}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doDriftGet(t, handler)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if len(provider.calls) != 1 {
		t.Fatalf("observes = %d, want exactly 1 (Observer is never retried)", len(provider.calls))
	}
}
