package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// applyBody builds a POST /v1/apply envelope of the same shape as planBody,
// with an extra trailing envelope fragment (e.g. a rollbackOnError field).
func applyBody(configResources, stateResources, extra string) string {
	return `{
	  "configuration": {
	    "resourceTypes": [{"name": "t", "schema": {"type": "object", "additionalProperties": true}}],
	    "resources": ` + configResources + `
	  },
	  "priorState": {
	    "resources": ` + stateResources + `
	  }` + extra + `
	}`
}

// multiApplyBody is the two-type counterpart of applyBody.
func multiApplyBody(configResources, stateResources, extra string) string {
	return `{
	  "configuration": {
	    "resourceTypes": [
	      {"name": "a", "schema": {"type": "object", "additionalProperties": true}},
	      {"name": "b", "schema": {"type": "object", "additionalProperties": true}}
	    ],
	    "resources": ` + configResources + `
	  },
	  "priorState": {
	    "resources": ` + stateResources + `
	  }` + extra + `
	}`
}

const rollbackFlag = `,
	  "rollbackOnError": true`

// rollbackProvider scripts failures by the full shape of a call, so the
// forward update (before v1 -> after v2) and its compensating update (before
// v2 -> after v1) are independently scriptable despite sharing action and
// address.
type rollbackProvider struct {
	mu       sync.Mutex
	failures map[string]map[int]error // signature -> 1-based attempts that fail
	calls    []ChangeRequest
}

func rollbackSig(req ChangeRequest) string {
	snap := func(s *Snapshot) string {
		if s == nil {
			return "-"
		}
		v := ""
		if s.Properties != nil {
			if raw, ok := s.Properties["v"]; ok {
				v = fmt.Sprintf("%v", raw)
			}
		}
		return s.Type + ":" + v
	}
	return req.Action + "|" + req.Address + "|" + snap(req.Before) + "->" + snap(req.After)
}

func (p *rollbackProvider) Apply(req ChangeRequest) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, req)
	if byAttempt, ok := p.failures[rollbackSig(req)]; ok {
		if err, fail := byAttempt[req.Attempt]; fail {
			return err
		}
	}
	return nil
}

func (p *rollbackProvider) callsLocked() []ChangeRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]ChangeRequest, len(p.calls))
	copy(out, p.calls)
	return out
}

// --- envelope validation ---

func TestApplyRollbackFlagRejectsNonBooleansWithoutCallingProvider(t *testing.T) {
	provider := &recordingProvider{}
	for _, value := range []string{`"yes"`, `1`, `null`, `[]`, `{}`} {
		body := applyBody(`[`+resEntry("a", `[]`)+`]`, `[]`, `, "rollbackOnError": `+value)
		res := doApply(t, HandlerWithProvider(provider), body, nil)
		if res.status != http.StatusUnprocessableEntity {
			t.Fatalf("rollbackOnError=%s: status = %d, want 422", value, res.status)
		}
		if res.Valid || len(res.Errors) != 1 {
			t.Fatalf("rollbackOnError=%s: payload = %+v", value, res)
		}
		e := res.Errors[0]
		if e.Code != codeInvalidPlanRequest || e.Path != "/rollbackOnError" {
			t.Fatalf("rollbackOnError=%s: error = %+v, want invalid_plan_request at /rollbackOnError", value, e)
		}
		if _, present := res.raw["applied"]; present {
			t.Fatalf("422 response must not contain applied: %v", res.raw)
		}
		if _, present := res.raw["rolledBack"]; present {
			t.Fatalf("422 response must not contain rolledBack: %v", res.raw)
		}
	}
	if len(provider.calls) != 0 {
		t.Fatalf("provider must not be called for an invalid flag, got %v", provider.calls)
	}
}

func TestApplyRollbackFlagTrueWithInvalidConfigurationReportsNormalErrors(t *testing.T) {
	provider := &recordingProvider{}
	// Unknown resource type x; the boolean flag must not mask normal errors.
	body := applyBody(`[{"address":"a","type":"x","properties":{}}]`, `[]`, rollbackFlag)
	res := doApply(t, HandlerWithProvider(provider), body, nil)
	if res.status != http.StatusUnprocessableEntity || res.Valid {
		t.Fatalf("status=%d payload=%+v", res.status, res)
	}
	found := false
	for _, e := range res.Errors {
		if e.Code == codeUnknownResourceType {
			found = true
		}
		if e.Path == "/rollbackOnError" {
			t.Fatalf("valid boolean must not be reported: %+v", e)
		}
	}
	if !found {
		t.Fatalf("expected an unknown_resource_type error, got %+v", res.Errors)
	}
	if len(provider.calls) != 0 {
		t.Fatalf("provider called for invalid request: %v", provider.calls)
	}
}

func TestPlanEndpointRejectsRollbackFlagAsUnknownField(t *testing.T) {
	// Other public entry points keep their envelope; the apply-only field is
	// unknown to POST /v1/plans.
	body := applyBody(`[`+resEntry("a", `[]`)+`]`, `[]`, rollbackFlag)
	rec := httptest.NewRecorder()
	HandlerWithProvider(&recordingProvider{}).ServeHTTP(rec,
		httptest.NewRequest(http.MethodPost, "/v1/plans", strings.NewReader(body)))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	var res validateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeInvalidPlanRequest || res.Errors[0].Path != "/rollbackOnError" {
		t.Fatalf("errors = %+v, want one unknown-field error at /rollbackOnError", res.Errors)
	}
}

func TestApplyBaselineResponsesOmitRollbackArrays(t *testing.T) {
	body := planBody(`[`+resEntry("a", `["b"]`)+`,`+resEntry("b", `[]`)+`]`, `[]`)

	t.Run("flag absent", func(t *testing.T) {
		res := doApply(t, HandlerWithProvider(&recordingProvider{failAt: 1, failErr: errors.New("boom at a")}), body, nil)
		if res.status != http.StatusBadGateway {
			t.Fatalf("status = %d", res.status)
		}
		if _, present := res.raw["rolledBack"]; present {
			t.Fatalf("baseline 502 must omit rolledBack: %v", res.raw)
		}
		if _, present := res.raw["rollbackErrors"]; present {
			t.Fatalf("baseline 502 must omit rollbackErrors: %v", res.raw)
		}
	})

	t.Run("flag false", func(t *testing.T) {
		res := doApply(t, HandlerWithProvider(&recordingProvider{failAt: 1, failErr: errors.New("boom at a")}), applyBody(
			`[`+resEntry("a", `["b"]`)+`,`+resEntry("b", `[]`)+`]`, `[]`, `, "rollbackOnError": false`), nil)
		if res.status != http.StatusBadGateway {
			t.Fatalf("status = %d", res.status)
		}
		if _, present := res.raw["rolledBack"]; present {
			t.Fatalf("rollbackOnError=false must omit rolledBack: %v", res.raw)
		}
		if res.Applied == nil || len(*res.Applied) != 1 || (*res.Applied)[0].Address != "b" {
			t.Fatalf("partial semantics changed: applied = %v", res.Applied)
		}
	})

	t.Run("baseline success", func(t *testing.T) {
		res := doApply(t, HandlerWithProvider(&recordingProvider{}),
			planBody(`[`+resEntry("a", `[]`)+`]`, `[]`), nil)
		if res.status != http.StatusOK {
			t.Fatalf("status = %d", res.status)
		}
		if _, present := res.raw["rolledBack"]; present {
			t.Fatalf("baseline 200 must omit rolledBack: %v", res.raw)
		}
		if _, present := res.raw["rollbackErrors"]; present {
			t.Fatalf("baseline 200 must omit rollbackErrors: %v", res.raw)
		}
	})
}

// --- success and empty plans ---

func TestApplyRollbackSuccessHasEmptyArraysAndKeepsOrder(t *testing.T) {
	provider := &recordingProvider{}
	body := applyBody(
		`[`+resEntry("c", `[]`)+`,`+resEntry("keep", `["new"]`)+`,`+resEntry("new", `[]`)+`]`,
		`[`+stateEntry("old", "t", `{}`, `["keep"]`)+`,`+stateEntry("keep", "t", `{"v": 1}`, ``)+`,`+stateEntry("c", "t", `{}`, ``)+`]`,
		rollbackFlag)
	res := doApply(t, HandlerWithProvider(provider), body, nil)
	if res.status != http.StatusOK || !res.Valid || len(res.Errors) != 0 {
		t.Fatalf("status=%d errors=%v", res.status, res.Errors)
	}
	if got := providerCallsSummary(provider.calls); got != "delete:old|create:new|update:keep" {
		t.Fatalf("calls = %q", got)
	}
	// No failure: no compensation runs, but both arrays are present and empty.
	if res.RolledBack == nil || len(*res.RolledBack) != 0 {
		t.Fatalf("rolledBack = %v, want present empty array", res.RolledBack)
	}
	if res.RollbackErrors == nil || len(*res.RollbackErrors) != 0 {
		t.Fatalf("rollbackErrors = %v, want present empty array", res.RollbackErrors)
	}
	// Forward-only calls: compensation keys minted but never used.
	if len(provider.calls) != 3 {
		t.Fatalf("provider calls = %v, want only the three forward changes", provider.calls)
	}
	if got := stateAddresses(res); got != "c|keep|new" {
		t.Fatalf("state = %q", got)
	}
}

func TestApplyRollbackEmptyPlanSucceedsWithoutProvider(t *testing.T) {
	res := doApply(t, Handler(), applyBody(`[]`, `[]`, rollbackFlag), nil)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("status=%d errors=%v", res.status, res.Errors)
	}
	if res.RolledBack == nil || res.RollbackErrors == nil {
		t.Fatalf("rollback arrays must be present: %+v", res.raw)
	}
}

func TestApplyRollbackWithoutProviderIsUnavailableAndOmitsArrays(t *testing.T) {
	res := doApply(t, Handler(), applyBody(`[`+resEntry("a", `[]`)+`]`, `[]`, rollbackFlag), nil)
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeProviderUnavailable || res.Errors[0].Path != "" {
		t.Fatalf("errors = %+v, want baseline single-provider 503", res.Errors)
	}
	if _, present := res.raw["rolledBack"]; present {
		t.Fatalf("pre-execution 503 must omit rolledBack: %v", res.raw)
	}
}

// --- compensation behavior ---

// fullCompensationFixture plans delete d, create b, create a (depends on b),
// update u (v1 -> v2), create z which fails forward; m and n are untouched
// noops; n's prior dependency list is intentionally unsorted so the restored
// state can be checked for normalization. Failing z at index 4 forces
// compensation of all four earlier successful changes.
func fullCompensationFixture() (string, *rollbackProvider) {
	config := `[` +
		resEntry("a", `["b"]`) + `,` +
		resEntry("b", `[]`) + `,` +
		resEntry("m", `[]`) + `,` +
		stateEntry("n", "t", `{}`, `["u","m"]`) + `,` +
		stateEntry("u", "t", `{"v": 2}`, `[]`) + `,` +
		resEntry("z", `[]`) + `]`
	prior := `[` +
		stateEntry("d", "t", `{}`, `[]`) + `,` +
		stateEntry("m", "t", `{}`, `[]`) + `,` +
		stateEntry("n", "t", `{}`, `["u","m"]`) + `,` +
		stateEntry("u", "t", `{"v": 1}`, `[]`) + `]`
	provider := &rollbackProvider{failures: map[string]map[int]error{
		// The trailing create z fails outright; everything before it
		// succeeds and must be compensated.
		"create|z|-->t:": {1: errors.New("z boom")},
	}}
	return applyBody(config, prior, rollbackFlag), provider
}

func TestApplyRollbackCompensatesAllSuccessesInReverse(t *testing.T) {
	body, provider := fullCompensationFixture()
	res := doApply(t, HandlerWithProvider(provider), body, nil)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}

	// Forward failure is still the single provider_error at the original
	// plan index with its final text.
	if len(res.Errors) != 1 {
		t.Fatalf("errors = %v, want exactly one", res.Errors)
	}
	e := res.Errors[0]
	if e.Code != codeProviderError || e.Message != "z boom" || e.Path != "/changes/4" {
		t.Fatalf("error = %+v", e)
	}

	calls := provider.callsLocked()
	wantCalls := []string{
		"delete:d", "create:b", "create:a", "update:u", "create:z", // forward
		"update:u", "delete:a", "delete:b", "create:d", // reverse compensation
	}
	gotCalls := make([]string, len(calls))
	for i, c := range calls {
		gotCalls[i] = c.Action + ":" + c.Address
	}
	if fmt.Sprint(gotCalls) != fmt.Sprint(wantCalls) {
		t.Fatalf("calls = %v, want %v", gotCalls, wantCalls)
	}

	// Compensation shapes: update swaps snapshots; create's compensation is
	// a delete carrying the created snapshot, delete's a create carrying the
	// removed snapshot.
	compUpdate := calls[5]
	if compUpdate.Before == nil || compUpdate.After == nil ||
		compUpdate.Before.Properties["v"] != json.Number("2") || compUpdate.After.Properties["v"] != json.Number("1") {
		t.Fatalf("compensating update did not swap snapshots: %+v", compUpdate)
	}
	if calls[6].Before == nil || calls[6].After != nil || calls[6].Before.Type != "t" {
		t.Fatalf("create a compensation should be delete with after snapshot: %+v", calls[6])
	}
	compCreateD := calls[8]
	if compCreateD.Before != nil || compCreateD.After == nil || compCreateD.After.Type != "t" {
		t.Fatalf("delete d compensation should be create with before snapshot: %+v", compCreateD)
	}

	// rolledBack follows the actual compensation order and lists the
	// original (forward) changes.
	if res.RolledBack == nil || len(*res.RolledBack) != 4 {
		t.Fatalf("rolledBack = %v, want four entries", res.RolledBack)
	}
	rb := *res.RolledBack
	wantRB := []string{"update:u", "create:a", "create:b", "delete:d"}
	for i, want := range wantRB {
		if got := rb[i].Action + ":" + rb[i].Address; got != want {
			t.Fatalf("rolledBack[%d] = %q, want %q; full %v", i, got, want, rb)
		}
	}
	if res.RollbackErrors == nil || len(*res.RollbackErrors) != 0 {
		t.Fatalf("rollbackErrors = %v, want empty", res.RollbackErrors)
	}

	// Everything was undone: applied empty and state equals the normalized
	// prior state, including untouched noops and ascending dependencies.
	if res.Applied == nil || len(*res.Applied) != 0 {
		t.Fatalf("applied = %v, want empty", res.Applied)
	}
	if got := stateAddresses(res); got != "d|m|n|u" {
		t.Fatalf("state = %q, want prior state d|m|n|u", got)
	}
	byAddress := map[string]stateResource{}
	for _, r := range res.State.Resources {
		byAddress[r.Address] = r
	}
	if v, ok := byAddress["u"].Properties["v"].(float64); !ok || v != 1 {
		t.Fatalf("u must be restored to before: %+v", byAddress["u"])
	}
	if got := strings.Join(byAddress["n"].DependsOn, "|"); got != "m|u" {
		t.Fatalf("n dependsOn = %v, want normalized [m u]", byAddress["n"].DependsOn)
	}
	if *res.Summary != (planSummary{Create: 3, Update: 1, Delete: 1, Noop: 2}) {
		t.Fatalf("summary changed through compensation: %+v", res.Summary)
	}
}

func TestApplyRollbackCompensationKeysAreUniqueAndReusedOnRetry(t *testing.T) {
	// b is created first; a's forward create fails plainly. Compensating b
	// fails once retryably, then succeeds, so its compensation runs attempts
	// 1 and 2 under one fresh, request-unique key.
	config := `[` + resEntry("a", `["b"]`) + `,` + resEntry("b", `[]`) + `]`
	provider := &rollbackProvider{failures: map[string]map[int]error{
		"create|a|-->t:": {1: errors.New("boom at a")},
		"delete|b|t:->-": {1: &retryableErr{msg: "comp transient", retryable: true}},
	}}
	res := doApply(t, HandlerWithProvider(provider), applyBody(config, `[]`, rollbackFlag), nil)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d", res.status)
	}
	calls := provider.callsLocked()
	want := []string{"create:b#1", "create:a#1", "delete:b#1", "delete:b#2"}
	got := make([]string, len(calls))
	for i, c := range calls {
		got[i] = fmt.Sprintf("%s:%s#%d", c.Action, c.Address, c.Attempt)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}

	keyB, keyA := calls[0].IdempotencyKey, calls[1].IdempotencyKey
	compKey := calls[2].IdempotencyKey
	for _, key := range []string{keyB, keyA, compKey} {
		if key == "" {
			t.Fatal("empty idempotency key")
		}
	}
	if compKey == keyB || compKey == keyA || keyA == keyB {
		t.Fatalf("keys not unique within the request: forward b=%q a=%q comp b=%q", keyB, keyA, compKey)
	}
	if calls[3].IdempotencyKey != compKey {
		t.Fatalf("compensation retry changed key: %q vs %q", calls[3].IdempotencyKey, compKey)
	}
	if res.Applied == nil || len(*res.Applied) != 0 {
		t.Fatalf("applied = %v, want empty after b was undone", res.Applied)
	}
	if got := stateAddresses(res); got != "" {
		t.Fatalf("state = %q, want empty", got)
	}
}

func TestApplyRollbackCompensationFailureIsRecordedAndSweepContinues(t *testing.T) {
	// Forward: b ok, a ok, c fails plainly. Compensation of a exhausts the
	// retry budget; b must still be compensated afterward.
	config := `[` + resEntry("b", `[]`) + `,` + resEntry("a", `[]`) + `,` + resEntry("c", `[]`) + `]`
	provider := &rollbackProvider{failures: map[string]map[int]error{
		"create|c|-->t:": {1: errors.New("boom at c")},
		"delete|a|t:->-": {
			1: &retryableErr{msg: "rb1", retryable: true},
			2: &retryableErr{msg: "rb2", retryable: true},
			3: &retryableErr{msg: "rb3", retryable: true},
		},
	}}
	res := doApply(t, HandlerWithProvider(provider), applyBody(config, `[]`, rollbackFlag), nil)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d", res.status)
	}
	if res.RollbackErrors == nil || len(*res.RollbackErrors) != 1 {
		t.Fatalf("rollbackErrors = %v, want one", res.RollbackErrors)
	}
	re := (*res.RollbackErrors)[0]
	if re.Code != codeRollbackError || re.Message != "rb3" || re.Path != "/rollbacks/0" {
		t.Fatalf("rollback error = %+v, want rb3 at /rollbacks/0", re)
	}
	// a failed to undo, b was still undone: rolledBack follows actual order.
	if res.RolledBack == nil || len(*res.RolledBack) != 1 || (*res.RolledBack)[0].Address != "b" {
		t.Fatalf("rolledBack = %v, want only [b]", res.RolledBack)
	}
	// applied keeps the residual still-effective change in original order;
	// state is prior plus exactly that residual.
	if res.Applied == nil || len(*res.Applied) != 1 || (*res.Applied)[0].Address != "a" {
		t.Fatalf("applied = %v, want only residual a", res.Applied)
	}
	if got := stateAddresses(res); got != "a" {
		t.Fatalf("state = %q, want only a", got)
	}
	// The forward error remains the sole errors entry.
	if len(res.Errors) != 1 || res.Errors[0].Path != "/changes/2" || res.Errors[0].Message != "boom at c" {
		t.Fatalf("errors = %+v", res.Errors)
	}
}

func TestApplyRollbackNonRetryableCompensationError(t *testing.T) {
	provider := &rollbackProvider{failures: map[string]map[int]error{
		"create|a|-->t:": {1: errors.New("forward boom")},
		"delete|b|t:->-": {1: errors.New("cannot undo")},
	}}
	body := applyBody(`[`+resEntry("a", `["b"]`)+`,`+resEntry("b", `[]`)+`]`, `[]`, rollbackFlag)
	res := doApply(t, HandlerWithProvider(provider), body, nil)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d", res.status)
	}
	if res.RollbackErrors == nil || len(*res.RollbackErrors) != 1 {
		t.Fatalf("rollbackErrors = %v", res.RollbackErrors)
	}
	re := (*res.RollbackErrors)[0]
	if re.Message != "cannot undo" || re.Path != "/rollbacks/0" {
		t.Fatalf("rollback error = %+v", re)
	}
	// A non-retryable compensation error spends exactly one attempt.
	var deleteAttempts []int
	for _, c := range provider.callsLocked() {
		if c.Action == "delete" {
			deleteAttempts = append(deleteAttempts, c.Attempt)
		}
	}
	if fmt.Sprint(deleteAttempts) != "[1]" {
		t.Fatalf("delete attempts = %v, want [1]", deleteAttempts)
	}
	// b stays applied.
	if res.Applied == nil || len(*res.Applied) != 1 || (*res.Applied)[0].Address != "b" {
		t.Fatalf("applied = %v, want residual b", res.Applied)
	}
}

func TestApplyRollbackFirstChangeFailureHasEmptyCompensation(t *testing.T) {
	provider := &rollbackProvider{failures: map[string]map[int]error{
		"create|a|-->t:": {1: errors.New("immediate boom")},
	}}
	res := doApply(t, HandlerWithProvider(provider),
		applyBody(`[`+resEntry("a", `[]`)+`]`, `[]`, rollbackFlag), nil)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d", res.status)
	}
	if res.RolledBack == nil || len(*res.RolledBack) != 0 {
		t.Fatalf("rolledBack = %v, want present empty", res.RolledBack)
	}
	if res.RollbackErrors == nil || len(*res.RollbackErrors) != 0 {
		t.Fatalf("rollbackErrors = %v, want present empty", res.RollbackErrors)
	}
	if res.Applied == nil || len(*res.Applied) != 0 {
		t.Fatalf("applied = %v, want empty", res.Applied)
	}
	if got := stateAddresses(res); got != "" {
		t.Fatalf("state = %q, want empty prior state", got)
	}
	if len(provider.callsLocked()) != 1 {
		t.Fatalf("calls = %v, only the failed forward change should run", provider.callsLocked())
	}
}

// cancelDuringCompensationProvider succeeds on every forward create except
// failCreate (plain error), and on the first compensation call cancels the
// request context and returns a retryable error, so the retry gate trips
// before attempt 2.
type cancelDuringCompensationProvider struct {
	cancel     context.CancelFunc
	failCreate string
	calls      []ChangeRequest
}

func (p *cancelDuringCompensationProvider) Apply(req ChangeRequest) error {
	p.calls = append(p.calls, req)
	if req.Action == "create" && req.Address == p.failCreate {
		return errors.New("forward boom")
	}
	if req.Action == "delete" {
		p.cancel()
		return &retryableErr{msg: "retry me", retryable: true}
	}
	return nil
}

func TestApplyRollbackCompensationAbortsRetryWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	provider := &cancelDuringCompensationProvider{cancel: cancel, failCreate: "a"}
	// a depends on b, so b succeeds first (index 0) and a (index 1) fails
	// forward; b is the lone success to compensate.
	body := applyBody(`[`+resEntry("a", `["b"]`)+`,`+resEntry("b", `[]`)+`]`, `[]`, rollbackFlag)
	res := doApply(t, HandlerWithProvider(provider), body, ctx)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d", res.status)
	}
	if res.RollbackErrors == nil || len(*res.RollbackErrors) != 1 {
		t.Fatalf("rollbackErrors = %v", res.RollbackErrors)
	}
	re := (*res.RollbackErrors)[0]
	if re.Code != codeRollbackError || re.Message != context.Canceled.Error() || re.Path != "/rollbacks/0" {
		t.Fatalf("rollback error = %+v, want context cancellation at /rollbacks/0", re)
	}
	// Exactly one compensation call: the context gate forbids attempt 2.
	deletes := 0
	for _, c := range provider.calls {
		if c.Action == "delete" {
			deletes++
		}
	}
	if deletes != 1 {
		t.Fatalf("compensation calls = %d, want exactly 1", deletes)
	}
	if res.Applied == nil || len(*res.Applied) != 1 || (*res.Applied)[0].Address != "b" {
		t.Fatalf("applied = %v, want residual b (its compensation did not finish)", res.Applied)
	}
}

// --- multi-Provider routing ---

func TestMultiApplyRollbackTypeChangingUpdateCompensatesThroughOriginalType(t *testing.T) {
	pa := &rollbackProvider{}
	pb := &rollbackProvider{failures: map[string]map[int]error{
		// The trailing create z fails, forcing compensation of the earlier
		// successful a -> b update through provider a.
		"create|z|-->b:": {1: errors.New("z boom")},
	}}
	handler := HandlerWithProviders(map[string]Provider{"a": pa, "b": pb})
	body := multiApplyBody(
		`[`+typedEntry("u", "b", `[]`)+`,`+typedEntry("z", "b", `[]`)+`]`,
		`[`+stateEntry("u", "a", `{}`, `[]`)+`]`, rollbackFlag)
	res := doApply(t, handler, body, nil)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Path != "/changes/1" {
		t.Fatalf("errors = %+v, want forward failure at /changes/1", res.Errors)
	}
	aCalls := pa.callsLocked()
	if len(aCalls) != 1 || aCalls[0].Action != "update" || aCalls[0].Address != "u" {
		t.Fatalf("original-type provider calls = %+v, want one compensating update", aCalls)
	}
	comp := aCalls[0]
	if comp.Before == nil || comp.Before.Type != "b" || comp.After == nil || comp.After.Type != "a" {
		t.Fatalf("compensation snapshots wrong: %+v", comp)
	}
	if res.Applied == nil || len(*res.Applied) != 0 {
		t.Fatalf("applied = %v, want empty", res.Applied)
	}
	byAddress := map[string]stateResource{}
	for _, r := range res.State.Resources {
		byAddress[r.Address] = r
	}
	if r, ok := byAddress["u"]; !ok || r.Type != "a" {
		t.Fatalf("state = %+v, want u restored as type a", byAddress)
	}
}

func TestMultiApplyRollbackMissingCompensationRouteFailsPrecheck(t *testing.T) {
	// Type-changing update a -> b: the forward routes to b (registered), the
	// compensation would route to a (not registered). Nothing may execute.
	pb := &rollbackProvider{}
	handler := HandlerWithProviders(map[string]Provider{"b": Provider(pb)})
	body := multiApplyBody(
		`[`+typedEntry("u", "b", `[]`)+`]`,
		`[`+stateEntry("u", "a", `{}`, `[]`)+`]`, rollbackFlag)
	res := doApply(t, handler, body, nil)
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.status)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("errors = %v, want one", res.Errors)
	}
	e := res.Errors[0]
	if e.Code != codeProviderUnavailable || e.Path != "/changes/0" || !strings.Contains(e.Message, `"a"`) {
		t.Fatalf("error = %+v, want provider_unavailable at /changes/0 naming type a", e)
	}
	if len(pb.callsLocked()) != 0 {
		t.Fatalf("provider must not be called: %v", pb.callsLocked())
	}
	if _, present := res.raw["rolledBack"]; present {
		t.Fatalf("pre-execution 503 must omit rolledBack: %v", res.raw)
	}
}

func TestMultiApplyRollbackMissingForwardRouteFailsPrecheck(t *testing.T) {
	pa := &rollbackProvider{}
	handler := HandlerWithProviders(map[string]Provider{"a": pa})
	body := multiApplyBody(`[`+typedEntry("u2", "b", `[]`)+`]`, `[]`, rollbackFlag)
	res := doApply(t, handler, body, nil)
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Path != "/changes/0" {
		t.Fatalf("errors = %+v", res.Errors)
	}
	if len(pa.callsLocked()) != 0 {
		t.Fatalf("provider called despite failed precheck")
	}
}

func TestMultiApplyRollbackRoutesCompensationsByType(t *testing.T) {
	// State order [old-a, old-b] -> deletes run old-b (b) then old-a (a);
	// create new-a (a) then fails forward. Compensation recreates old-a via a
	// and old-b via b; the residual applied list is empty.
	pa := &rollbackProvider{failures: map[string]map[int]error{
		"create|new-a|-->a:": {1: errors.New("new boom")},
	}}
	pb := &rollbackProvider{}
	handler := HandlerWithProviders(map[string]Provider{"a": pa, "b": pb})
	body := multiApplyBody(
		`[`+typedEntry("new-a", "a", `[]`)+`]`,
		`[`+stateEntry("old-a", "a", `{}`, `[]`)+`,`+stateEntry("old-b", "b", `{}`, `[]`)+`]`,
		rollbackFlag)
	res := doApply(t, handler, body, nil)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d", res.status)
	}
	if got := providerCallsSummary(pa.callsLocked()); got != "delete:old-a|create:new-a|create:old-a" {
		t.Fatalf("provider a calls = %q", got)
	}
	if got := providerCallsSummary(pb.callsLocked()); got != "delete:old-b|create:old-b" {
		t.Fatalf("provider b calls = %q", got)
	}
	if res.RolledBack == nil || len(*res.RolledBack) != 2 {
		t.Fatalf("rolledBack = %v", res.RolledBack)
	}
	rb := *res.RolledBack
	if rb[0].Action+":"+rb[0].Address != "delete:old-a" || rb[1].Action+":"+rb[1].Address != "delete:old-b" {
		t.Fatalf("rolledBack order = %v", rb)
	}
	if got := stateAddresses(res); got != "old-a|old-b" {
		t.Fatalf("state = %q, want prior state restored", got)
	}
}

func TestMultiApplyRollbackAllSuccessRoutedAndEmptyArrays(t *testing.T) {
	pa := &rollbackProvider{}
	pb := &rollbackProvider{}
	handler := HandlerWithProviders(map[string]Provider{"a": pa, "b": pb})
	body := multiApplyBody(
		`[`+typedEntry("u1", "a", `[]`)+`,`+typedEntry("u2", "b", `["u1"]`)+`]`,
		`[]`, rollbackFlag)
	res := doApply(t, handler, body, nil)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("status=%d errors=%v", res.status, res.Errors)
	}
	if len(pa.callsLocked()) != 1 || len(pb.callsLocked()) != 1 {
		t.Fatalf("forward routing changed: a=%v b=%v", pa.callsLocked(), pb.callsLocked())
	}
	if res.RolledBack == nil || len(*res.RolledBack) != 0 ||
		res.RollbackErrors == nil || len(*res.RollbackErrors) != 0 {
		t.Fatalf("rollback arrays = %+v %+v", res.RolledBack, res.RollbackErrors)
	}
}
