package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// retryableErr is a RetryableError whose retryability is fixed at
// construction, matching the interface a Provider would return to ask the
// server to attempt the same change again.
type retryableErr struct {
	msg       string
	retryable bool
}

func (e *retryableErr) Error() string { return e.msg }
func (e *retryableErr) Retryable() bool {
	return e.retryable
}

// wrappedRetryableErr wraps another error while still implementing
// RetryableError, exercising errors.As through an Unwrap chain.
type wrappedRetryableErr struct {
	inner error
}

func (e *wrappedRetryableErr) Error() string { return "wrapped: " + e.inner.Error() }
func (e *wrappedRetryableErr) Retryable() bool {
	return true
}
func (e *wrappedRetryableErr) Unwrap() error { return e.inner }

// scriptedRetryProvider fails per (address, attempt) with scripted errors and
// records every Apply call with its idempotency key and attempt number.
type scriptedRetryProvider struct {
	mu       sync.Mutex
	failures map[string]map[int]error // address -> 1-based attempts that fail
	calls    []ChangeRequest
}

func (p *scriptedRetryProvider) Apply(req ChangeRequest) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, req)
	if byAttempt, ok := p.failures[req.Address]; ok {
		if err, fail := byAttempt[req.Attempt]; fail {
			return err
		}
	}
	return nil
}

func (p *scriptedRetryProvider) callsLocked() []ChangeRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]ChangeRequest, len(p.calls))
	copy(out, p.calls)
	return out
}

func attemptsFor(calls []ChangeRequest, address string) []int {
	var attempts []int
	for _, c := range calls {
		if c.Address == address {
			attempts = append(attempts, c.Attempt)
		}
	}
	return attempts
}

func keysFor(calls []ChangeRequest, address string) []string {
	var keys []string
	for _, c := range calls {
		if c.Address == address {
			keys = append(keys, c.IdempotencyKey)
		}
	}
	return keys
}

// TestApplyRetrySucceedsOnThirdAttempt verifies the core retry contract: two
// retryable failures followed by success yield attempts 1, 2, 3 with one
// shared idempotency key, record the change once, and succeed overall.
func TestApplyRetrySucceedsOnThirdAttempt(t *testing.T) {
	body := planBody(`[`+resEntry("a", `[]`)+`]`, `[]`)
	transient := &retryableErr{msg: "transient", retryable: true}
	provider := &scriptedRetryProvider{failures: map[string]map[int]error{
		"a": {1: transient, 2: transient},
	}}
	res := doApply(t, HandlerWithProvider(provider), body, nil)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
	calls := provider.callsLocked()
	if got := attemptsFor(calls, "a"); fmt.Sprint(got) != "[1 2 3]" {
		t.Fatalf("attempts = %v, want [1 2 3]", got)
	}
	keys := keysFor(calls, "a")
	if len(keys) != 3 || keys[0] == "" || keys[0] != keys[1] || keys[1] != keys[2] {
		t.Fatalf("idempotency keys = %v, want one non-empty key on all attempts", keys)
	}
	if res.Applied == nil || len(*res.Applied) != 1 || (*res.Applied)[0].Address != "a" {
		t.Fatalf("applied = %v, want a recorded exactly once", res.Applied)
	}
}

// TestApplyRetryExhaustedReportsLastErrorAndIndex verifies that three
// retryable failures stop with 502 provider_error carrying the last error
// text and the change's global plan index.
func TestApplyRetryExhaustedReportsLastErrorAndIndex(t *testing.T) {
	// b is created first (dependency), a second; a's index in the plan is 1.
	body := planBody(`[`+resEntry("a", `["b"]`)+`,`+resEntry("b", `[]`)+`]`, `[]`)
	provider := &scriptedRetryProvider{failures: map[string]map[int]error{
		"a": {
			1: &retryableErr{msg: "try 1", retryable: true},
			2: &retryableErr{msg: "try 2", retryable: true},
			3: &retryableErr{msg: "try 3", retryable: true},
		},
	}}
	res := doApply(t, HandlerWithProvider(provider), body, nil)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("errors = %v, want one", res.Errors)
	}
	e := res.Errors[0]
	if e.Code != codeProviderError || e.Message != "try 3" || e.Path != "/changes/1" {
		t.Fatalf("error = %+v, want last text at /changes/1", e)
	}
	calls := provider.callsLocked()
	if got := attemptsFor(calls, "b"); fmt.Sprint(got) != "[1]" {
		t.Fatalf("b attempts = %v, want single success", got)
	}
	if got := attemptsFor(calls, "a"); fmt.Sprint(got) != "[1 2 3]" {
		t.Fatalf("a attempts = %v, want [1 2 3]", got)
	}
	// b applied once, a never recorded.
	if res.Applied == nil || len(*res.Applied) != 1 || (*res.Applied)[0].Address != "b" {
		t.Fatalf("applied = %v, want only b", res.Applied)
	}
}

// TestApplyNonRetryableErrorKeepsBaselineBehavior verifies that a plain error
// and a Retryable() == false error each stop after a single Apply call.
func TestApplyNonRetryableErrorKeepsBaselineBehavior(t *testing.T) {
	body := planBody(`[`+resEntry("a", `[]`)+`]`, `[]`)

	t.Run("plain error", func(t *testing.T) {
		provider := &scriptedRetryProvider{failures: map[string]map[int]error{
			"a": {1: errors.New("plain boom")},
		}}
		res := doApply(t, HandlerWithProvider(provider), body, nil)
		if res.status != http.StatusBadGateway || res.Errors[0].Message != "plain boom" {
			t.Fatalf("unexpected result: %d %+v", res.status, res.Errors)
		}
		if got := attemptsFor(provider.callsLocked(), "a"); len(got) != 1 {
			t.Fatalf("attempts = %v, want exactly 1", got)
		}
	})

	t.Run("explicitly non-retryable", func(t *testing.T) {
		provider := &scriptedRetryProvider{failures: map[string]map[int]error{
			"a": {1: &retryableErr{msg: "fatal", retryable: false}},
		}}
		res := doApply(t, HandlerWithProvider(provider), body, nil)
		if res.status != http.StatusBadGateway || res.Errors[0].Message != "fatal" {
			t.Fatalf("unexpected result: %d %+v", res.status, res.Errors)
		}
		if got := attemptsFor(provider.callsLocked(), "a"); len(got) != 1 {
			t.Fatalf("attempts = %v, want exactly 1", got)
		}
	})
}

// TestApplyRetryableThenNonRetryableStops verifies that a later attempt
// becoming non-retryable stops the run immediately without spending the
// remaining budget, reporting that attempt's error text.
func TestApplyRetryableThenNonRetryableStops(t *testing.T) {
	body := planBody(`[`+resEntry("a", `[]`)+`]`, `[]`)
	provider := &scriptedRetryProvider{failures: map[string]map[int]error{
		"a": {
			1: &retryableErr{msg: "transient", retryable: true},
			2: errors.New("now fatal"),
		},
	}}
	res := doApply(t, HandlerWithProvider(provider), body, nil)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if e := res.Errors[0]; e.Message != "now fatal" || e.Path != "/changes/0" {
		t.Fatalf("error = %+v", e)
	}
	if got := attemptsFor(provider.callsLocked(), "a"); fmt.Sprint(got) != "[1 2]" {
		t.Fatalf("attempts = %v, want [1 2] — no third attempt", got)
	}
}

// TestApplyWrappedRetryableErrorIsRecognized verifies errors.As unwrapping:
// a wrapped error implementing RetryableError is still retried.
func TestApplyWrappedRetryableErrorIsRecognized(t *testing.T) {
	body := planBody(`[`+resEntry("a", `[]`)+`]`, `[]`)
	provider := &scriptedRetryProvider{failures: map[string]map[int]error{
		"a": {1: &wrappedRetryableErr{inner: errors.New("x")}},
	}}
	res := doApply(t, HandlerWithProvider(provider), body, nil)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("status=%d errors=%v", res.status, res.Errors)
	}
	if got := attemptsFor(provider.callsLocked(), "a"); fmt.Sprint(got) != "[1 2]" {
		t.Fatalf("attempts = %v, want wrapped retryable error retried once", got)
	}
}

// TestApplyIdempotencyKeysAreUniqueWithinRequestAndFreshAcrossRequests
// verifies the key allocation rules: non-empty, distinct per change within a
// request, reused across that change's attempts, and never reused by a later
// top-level request.
func TestApplyIdempotencyKeysAreUniqueWithinRequestAndFreshAcrossRequests(t *testing.T) {
	body := planBody(`[`+resEntry("a", `[]`)+`,`+resEntry("b", `[]`)+`,`+resEntry("c", `[]`)+`]`, `[]`)

	provider := &scriptedRetryProvider{failures: map[string]map[int]error{
		"b": {1: &retryableErr{msg: "t", retryable: true}}, // b retries once
	}}
	handler := HandlerWithProvider(provider)

	first := doApply(t, handler, body, nil)
	if first.status != http.StatusOK {
		t.Fatalf("first status = %d errors=%v", first.status, first.Errors)
	}
	second := doApply(t, handler, body, nil)
	if second.status != http.StatusOK {
		t.Fatalf("second status = %d errors=%v", second.status, second.Errors)
	}

	calls := provider.callsLocked()
	// Each request runs a#1, b#1, b#2 (b fails once), c#1: 4 calls apiece.
	// The failures persist across requests, which is what lets us verify the
	// retried change still gets a fresh key in the second request.
	if len(calls) != 8 {
		t.Fatalf("calls = %d, want 8 (a,b,b,c per request)", len(calls))
	}
	firstBatch, secondBatch := calls[:4], calls[4:]

	checkBatch := func(batch []ChangeRequest) map[string]string {
		byAddress := map[string]string{}
		seen := map[string]bool{}
		for _, c := range batch {
			if c.IdempotencyKey == "" {
				t.Fatalf("empty idempotency key in call %+v", c)
			}
			if c.Attempt < 1 {
				t.Fatalf("attempt = %d, want >= 1", c.Attempt)
			}
			if existing, ok := byAddress[c.Address]; ok {
				if existing != c.IdempotencyKey {
					t.Fatalf("address %s reused a different key across attempts: %q vs %q", c.Address, existing, c.IdempotencyKey)
				}
			} else {
				if seen[c.IdempotencyKey] {
					t.Fatalf("key %q shared by two changes in one request", c.IdempotencyKey)
				}
				seen[c.IdempotencyKey] = true
				byAddress[c.Address] = c.IdempotencyKey
			}
		}
		return byAddress
	}

	firstKeys := checkBatch(firstBatch)
	secondKeys := checkBatch(secondBatch)
	for address, key := range firstKeys {
		if key == secondKeys[address] {
			t.Fatalf("address %s reused key %q across top-level requests", address, key)
		}
	}
}

// TestApplyRetryDoesNotAdvanceToLaterChanges verifies that while a change is
// retrying, no later change is touched; subsequent changes execute only after
// the retried change succeeds.
func TestApplyRetryDoesNotAdvanceToLaterChanges(t *testing.T) {
	body := planBody(`[`+resEntry("a", `[]`)+`,`+resEntry("b", `[]`)+`]`, `[]`)
	provider := &scriptedRetryProvider{failures: map[string]map[int]error{
		"a": {1: &retryableErr{msg: "t", retryable: true}},
	}}
	res := doApply(t, HandlerWithProvider(provider), body, nil)
	if res.status != http.StatusOK {
		t.Fatalf("status = %d", res.status)
	}
	calls := provider.callsLocked()
	got := make([]string, len(calls))
	for i, c := range calls {
		got[i] = fmt.Sprintf("%s#%d", c.Address, c.Attempt)
	}
	if want := []string{"a#1", "a#2", "b#1"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("call sequence = %v, want %v", got, want)
	}
}

// TestApplyRetryAbortsWhenContextEndsBeforeNextAttempt verifies that once the
// request context is over, a retryable failure is not followed by another
// Provider call; the response carries context.Err()'s text as provider_error.
func TestApplyRetryAbortsWhenContextEndsBeforeNextAttempt(t *testing.T) {
	body := planBody(`[`+resEntry("a", `[]`)+`]`, `[]`)
	ctx, cancel := context.WithCancel(context.Background())
	provider := &cancelOnRetryProvider{cancel: cancel}
	res := doApply(t, HandlerWithProvider(provider), body, ctx)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if e := res.Errors[0]; e.Code != codeProviderError || e.Message != context.Canceled.Error() || e.Path != "/changes/0" {
		t.Fatalf("error = %+v, want context cancellation text at /changes/0", e)
	}
	if len(provider.calls) != 1 {
		t.Fatalf("provider calls = %d, want exactly 1 (no retry after context ended)", len(provider.calls))
	}
	if provider.calls[0].Attempt != 1 || provider.calls[0].IdempotencyKey == "" {
		t.Fatalf("first call shape wrong: %+v", provider.calls[0])
	}
}

// cancelOnRetryProvider returns a retryable error on the first attempt and
// cancels the request context at the same time, so the retry gate trips.
type cancelOnRetryProvider struct {
	cancel context.CancelFunc
	calls  []ChangeRequest
}

func (p *cancelOnRetryProvider) Apply(req ChangeRequest) error {
	p.calls = append(p.calls, req)
	p.cancel()
	return &retryableErr{msg: "should not be retried", retryable: true}
}

// --- POST /v1/state/apply retry semantics ---

func TestStateApplyRetriesAndCommitsOnce(t *testing.T) {
	path := stateFilePath(t)
	provider := &scriptedRetryProvider{failures: map[string]map[int]error{
		"a": {1: &retryableErr{msg: "transient", retryable: true}},
	}}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doStateApply(t, handler, stateConfigBody(`[`+resEntry("a", `[]`)+`]`))
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("status=%d errors=%v", res.status, res.Errors)
	}
	calls := provider.callsLocked()
	if got := attemptsFor(calls, "a"); fmt.Sprint(got) != "[1 2]" {
		t.Fatalf("attempts = %v, want [1 2]", got)
	}
	if res.Applied == nil || len(*res.Applied) != 1 {
		t.Fatalf("applied = %v, want a recorded once", res.Applied)
	}
	// One successful run equals one commit and one revision increment even
	// though the Provider was called twice.
	if res.Revision == nil || *res.Revision != 1 {
		t.Fatalf("revision = %v, want 1", res.Revision)
	}
	if revision, _ := readStateFileRaw(t, path); revision != 1 {
		t.Fatalf("file revision = %d, want 1", revision)
	}
}

func TestStateApplyRetryExhaustedLeavesFileUntouched(t *testing.T) {
	path := stateFilePath(t)
	provider := &scriptedRetryProvider{failures: map[string]map[int]error{
		"a": {
			1: &retryableErr{msg: "e1", retryable: true},
			2: &retryableErr{msg: "e2", retryable: true},
			3: &retryableErr{msg: "e3", retryable: true},
		},
	}}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doStateApply(t, handler, stateConfigBody(`[`+resEntry("a", `[]`)+`]`))
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if e := res.Errors[0]; e.Code != codeProviderError || e.Message != "e3" || e.Path != "/changes/0" {
		t.Fatalf("error = %+v, want e3 at /changes/0", e)
	}
	if got := attemptsFor(provider.callsLocked(), "a"); fmt.Sprint(got) != "[1 2 3]" {
		t.Fatalf("attempts = %v, want [1 2 3]", got)
	}
	if res.Revision == nil || *res.Revision != 0 {
		t.Fatalf("revision = %v, want 0 (first-change failure keeps file untouched)", res.Revision)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("state file should never have been created, stat err = %v", err)
	}
}

func TestStateApplyRetryExhaustedAfterSuccessCommitsPartial(t *testing.T) {
	path := stateFilePath(t)
	// b is applied first; a retries to exhaustion afterward.
	provider := &scriptedRetryProvider{failures: map[string]map[int]error{
		"a": {
			1: &retryableErr{msg: "e1", retryable: true},
			2: &retryableErr{msg: "e2", retryable: true},
			3: &retryableErr{msg: "e3", retryable: true},
		},
	}}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doStateApply(t, handler,
		stateConfigBody(`[`+resEntry("a", `["b"]`)+`,`+resEntry("b", `[]`)+`]`))
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if e := res.Errors[0]; e.Message != "e3" || e.Path != "/changes/1" {
		t.Fatalf("error = %+v, want e3 at /changes/1", e)
	}
	if res.Applied == nil || len(*res.Applied) != 1 || (*res.Applied)[0].Address != "b" {
		t.Fatalf("applied = %v, want only b", res.Applied)
	}
	if res.Revision == nil || *res.Revision != 1 {
		t.Fatalf("revision = %v, want 1 (partial commit)", res.Revision)
	}
	revision, resources := readStateFileRaw(t, path)
	if revision != 1 || len(resources) != 1 || resources[0].Address != "b" {
		t.Fatalf("file = rev %d resources %+v, want rev 1 with only b", revision, resources)
	}
}

func TestStateApplyNonRetryableErrorIsNotRetried(t *testing.T) {
	path := stateFilePath(t)
	provider := &scriptedRetryProvider{failures: map[string]map[int]error{
		"a": {1: errors.New("hard stop")},
	}}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doStateApply(t, handler, stateConfigBody(`[`+resEntry("a", `[]`)+`]`))
	if res.status != http.StatusBadGateway || res.Errors[0].Message != "hard stop" {
		t.Fatalf("unexpected result: %d %+v", res.status, res.Errors)
	}
	if got := attemptsFor(provider.callsLocked(), "a"); len(got) != 1 {
		t.Fatalf("attempts = %v, want exactly 1", got)
	}
}

// lockCheckingRetryProvider asserts on every Apply call that the state lock
// for path is still held by the in-flight run, fails retryably once, and
// succeeds on the second call.
type lockCheckingRetryProvider struct {
	path    string
	checked int
	calls   []ChangeRequest
}

func (p *lockCheckingRetryProvider) Apply(req ChangeRequest) error {
	p.calls = append(p.calls, req)
	if unlock, ok := tryLockState(newStateBackend(p.path).path); ok {
		unlock()
		panic("state lock was not held during Apply")
	}
	p.checked++
	if p.checked == 1 {
		return &retryableErr{msg: "once", retryable: true}
	}
	return nil
}

func TestStateApplyLockHeldAcrossRetries(t *testing.T) {
	path := stateFilePath(t)
	provider := &lockCheckingRetryProvider{path: path}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doStateApply(t, handler, stateConfigBody(`[`+resEntry("a", `[]`)+`]`))
	if res.status != http.StatusOK {
		t.Fatalf("status = %d errors=%v", res.status, res.Errors)
	}
	if len(provider.calls) != 2 || provider.calls[0].IdempotencyKey != provider.calls[1].IdempotencyKey {
		t.Fatalf("calls = %+v, want two attempts sharing one key", provider.calls)
	}
	// The run released the lock after finishing: it can be taken again.
	unlock, ok := tryLockState(newStateBackend(path).path)
	if !ok {
		t.Fatal("state lock was not released after the run")
	}
	unlock()
}

func TestStateApplyRetryAbortsWhenContextEnds(t *testing.T) {
	path := stateFilePath(t)
	ctx, cancel := context.WithCancel(context.Background())
	provider := &cancelOnRetryProvider{cancel: cancel}
	handler := HandlerWithProviderAndStateFile(provider, path)

	req := httptest.NewRequest(http.MethodPost, "/v1/state/apply",
		strings.NewReader(stateConfigBody(`[`+resEntry("a", `[]`)+`]`)))
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var res stateApplyResult
	res.status = rec.Code
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if e := res.Errors[0]; e.Code != codeProviderError || e.Message != context.Canceled.Error() || e.Path != "/changes/0" {
		t.Fatalf("error = %+v", e)
	}
	if len(provider.calls) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(provider.calls))
	}
}

// --- POST /v1/state/reconcile retry semantics ---

// scriptedReconcileProvider is a scriptedRetryProvider that also implements
// Observer with a fixed snapshot map.
type scriptedReconcileProvider struct {
	scriptedRetryProvider
	snapshots map[string]*Snapshot
}

func (p *scriptedReconcileProvider) Observe(_ context.Context, address string) (*Snapshot, error) {
	return p.snapshots[address], nil
}

func TestReconcileRetriesChangeAndKeepsRevision(t *testing.T) {
	path := stateFilePath(t)
	// c is saved but absent remotely -> create; it fails once retryably.
	writeDriftStateFile(t, path, 9, `[
	  {"address": "c", "type": "t", "properties": {}}
	]`)
	provider := &scriptedReconcileProvider{
		snapshots: map[string]*Snapshot{},
		scriptedRetryProvider: scriptedRetryProvider{failures: map[string]map[int]error{
			"c": {1: &retryableErr{msg: "transient", retryable: true}},
		}},
	}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doReconcile(t, handler, `{}`)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("status=%d errors=%v", res.status, res.Errors)
	}
	if got := attemptsFor(provider.callsLocked(), "c"); fmt.Sprint(got) != "[1 2]" {
		t.Fatalf("attempts = %v, want [1 2]", got)
	}
	if res.Applied == nil || len(*res.Applied) != 1 || (*res.Applied)[0].Address != "c" {
		t.Fatalf("applied = %v, want c once", res.Applied)
	}
	if res.Revision == nil || *res.Revision != 9 {
		t.Fatalf("revision = %v, want unchanged 9", res.Revision)
	}
	if got := fileContent(t, path); !strings.Contains(got, `"revision": 9`) {
		t.Fatalf("state file was rewritten: %s", got)
	}
}

func TestReconcileRetryExhaustedReportsLastError(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 3, `[
	  {"address": "c", "type": "t", "properties": {}}
	]`)
	provider := &scriptedReconcileProvider{
		snapshots: map[string]*Snapshot{},
		scriptedRetryProvider: scriptedRetryProvider{failures: map[string]map[int]error{
			"c": {
				1: &retryableErr{msg: "r1", retryable: true},
				2: &retryableErr{msg: "r2", retryable: true},
				3: &retryableErr{msg: "r3", retryable: true},
			},
		}},
	}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doReconcile(t, handler, `{}`)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if e := res.Errors[0]; e.Code != codeProviderError || e.Message != "r3" || e.Path != "/changes/0" {
		t.Fatalf("error = %+v, want r3 at /changes/0", e)
	}
	if got := attemptsFor(provider.callsLocked(), "c"); fmt.Sprint(got) != "[1 2 3]" {
		t.Fatalf("attempts = %v, want [1 2 3]", got)
	}
	if res.Applied == nil || len(*res.Applied) != 0 {
		t.Fatalf("applied = %v, want empty", res.Applied)
	}
	if res.Revision == nil || *res.Revision != 3 {
		t.Fatalf("revision = %v, want 3", res.Revision)
	}
}

func TestReconcileNonRetryableErrorIsNotRetried(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 1, `[
	  {"address": "c", "type": "t", "properties": {}}
	]`)
	provider := &scriptedReconcileProvider{
		snapshots:             map[string]*Snapshot{},
		scriptedRetryProvider: scriptedRetryProvider{failures: map[string]map[int]error{"c": {1: errors.New("nope")}}},
	}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doReconcile(t, handler, `{}`)
	if res.status != http.StatusBadGateway || res.Errors[0].Message != "nope" {
		t.Fatalf("unexpected: %d %+v", res.status, res.Errors)
	}
	if got := attemptsFor(provider.callsLocked(), "c"); len(got) != 1 {
		t.Fatalf("attempts = %v, want exactly 1", got)
	}
}

// --- type-dispatched providers follow the same retry policy ---

func TestMultiApplyRetriesThroughDispatchedProvider(t *testing.T) {
	pa := &scriptedRetryProvider{failures: map[string]map[int]error{
		"u1": {1: &retryableErr{msg: "t", retryable: true}},
	}}
	pb := &scriptedRetryProvider{}
	handler := HandlerWithProviders(map[string]Provider{"a": pa, "b": pb})

	body := multiPlanBody(
		`[`+typedEntry("u1", "a", `[]`)+`,`+typedEntry("u2", "b", `[]`)+`]`,
		`[]`)
	res := doApply(t, handler, body, nil)
	if res.status != http.StatusOK {
		t.Fatalf("status = %d errors=%v", res.status, res.Errors)
	}
	if got := attemptsFor(pa.callsLocked(), "u1"); fmt.Sprint(got) != "[1 2]" {
		t.Fatalf("provider a attempts = %v, want [1 2]", got)
	}
	keys := keysFor(pa.callsLocked(), "u1")
	if keys[0] == "" || keys[0] != keys[1] {
		t.Fatalf("keys = %v, want one stable key", keys)
	}
	if got := attemptsFor(pb.callsLocked(), "u2"); fmt.Sprint(got) != "[1]" {
		t.Fatalf("provider b attempts = %v, want [1] — not touched during a's retry", got)
	}
	// Dispatched providers receive distinct keys within the one request.
	if keys[0] == pb.callsLocked()[0].IdempotencyKey {
		t.Fatal("the two changes shared an idempotency key")
	}
}

func TestMultiStateApplyRetriesThroughDispatchedProvider(t *testing.T) {
	path := stateFilePath(t)
	pa := &scriptedRetryProvider{failures: map[string]map[int]error{
		"u1": {
			1: &retryableErr{msg: "r1", retryable: true},
			2: &retryableErr{msg: "r2", retryable: true},
			3: &retryableErr{msg: "r3", retryable: true},
		},
	}}
	pb := &scriptedRetryProvider{}
	handler := HandlerWithProvidersAndStateFile(map[string]Provider{"a": pa, "b": pb}, path)

	res := doStateApply(t, handler, multiStateConfigBody(
		`[`+typedEntry("u1", "a", `[]`)+`,`+typedEntry("u2", "b", `[]`)+`]`))
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if e := res.Errors[0]; e.Message != "r3" || e.Path != "/changes/0" {
		t.Fatalf("error = %+v", e)
	}
	if len(pb.callsLocked()) != 0 {
		t.Fatalf("provider b must not be called while u1 is retrying: %v", pb.callsLocked())
	}
}

// --- read-only paths never retry ---

// observerRetryableErr counts how often Observe is called for an error that
// does implement RetryableError; the read-only drift path must still call it
// exactly once and answer observer_error without an Apply.
func TestDriftObserveErrorIsNeverRetriedEvenWhenRetryable(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 5, `[
	  {"address": "c", "type": "t", "properties": {}}
	]`)
	provider := &countingObserverProvider{err: &retryableErr{msg: "observe boom", retryable: true}}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doDriftGet(t, handler)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if provider.observes != 1 {
		t.Fatalf("Observe calls = %d, want exactly 1 (no retries on read path)", provider.observes)
	}
	if provider.applies != 0 {
		t.Fatalf("Apply calls = %d, want 0 on drift", provider.applies)
	}
}

type countingObserverProvider struct {
	err      error
	observes int
	applies  int
}

func (p *countingObserverProvider) Apply(ChangeRequest) error {
	p.applies++
	return nil
}

func (p *countingObserverProvider) Observe(context.Context, string) (*Snapshot, error) {
	p.observes++
	return nil, p.err
}
