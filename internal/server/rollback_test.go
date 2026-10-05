package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// compensationScriptProvider scripts Apply failures by action and address,
// so a forward change and its compensation — which share the address but
// restart the attempt count — can be scripted independently.
type compensationScriptProvider struct {
	calls    []ChangeRequest
	failures map[string]map[int]error // action+":"+address -> 1-based attempts that fail
}

func (p *compensationScriptProvider) Apply(req ChangeRequest) error {
	p.calls = append(p.calls, req)
	if byAttempt, ok := p.failures[req.Action+":"+req.Address]; ok {
		if err, fail := byAttempt[req.Attempt]; fail {
			return err
		}
	}
	return nil
}

// rollbackBody wraps a configuration resources fragment and a priorState
// resources fragment in an apply request carrying the given rollbackOnError
// literal (e.g. "true", "false", or a non-boolean JSON value).
func rollbackBody(configResources, stateResources, rollbackFlag string) string {
	body := planBody(configResources, stateResources)
	return body[:len(body)-1] + `, "rollbackOnError": ` + rollbackFlag + `}`
}

// rolledBackSummary renders the rolledBack list like providerCallsSummary
// renders provider calls.
func rolledBackSummary(changes []planChange) string {
	parts := make([]string, 0, len(changes))
	for _, c := range changes {
		parts = append(parts, c.Action+":"+c.Address)
	}
	return strings.Join(parts, "|")
}

func TestApplyRollbackOnErrorNonBooleanIs422(t *testing.T) {
	provider := &recordingProvider{}
	for _, flag := range []string{`"yes"`, `1`, `0`, `{}`, `[]`, `null`} {
		body := rollbackBody(`[`+resEntry("a", `[]`)+`]`, `[]`, flag)
		res := doApply(t, HandlerWithProvider(provider), body, nil)
		if res.status != http.StatusUnprocessableEntity {
			t.Fatalf("flag %s: status = %d, want 422", flag, res.status)
		}
		if res.Valid || len(res.Errors) != 1 {
			t.Fatalf("flag %s: payload = %+v", flag, res)
		}
		e := res.Errors[0]
		if e.Code != codeInvalidPlanRequest || e.Path != "/rollbackOnError" {
			t.Fatalf("flag %s: error = %+v, want invalid_plan_request at /rollbackOnError", flag, e)
		}
		if _, present := res.raw["applied"]; present {
			t.Fatalf("flag %s: 422 response must not contain applied: %v", flag, res.raw)
		}
	}
	if len(provider.calls) != 0 {
		t.Fatalf("provider must not be called, got %v", providerCallsSummary(provider.calls))
	}
}

func TestApplyRollbackOnErrorFalseKeepsBaselineSemantics(t *testing.T) {
	// b is created first, a second and fails: partial apply, no compensation.
	body := rollbackBody(`[`+resEntry("a", `["b"]`)+`,`+resEntry("b", `[]`)+`]`, `[]`, `false`)
	provider := &recordingProvider{failAt: 1, failErr: errors.New("boom at a")}
	res := doApply(t, HandlerWithProvider(provider), body, nil)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if _, present := res.raw["rolledBack"]; present {
		t.Fatalf("rollbackOnError=false must not add rolledBack: %v", res.raw)
	}
	if _, present := res.raw["rollbackErrors"]; present {
		t.Fatalf("rollbackOnError=false must not add rollbackErrors: %v", res.raw)
	}
	if got := providerCallsSummary(provider.calls); got != "create:b|create:a" {
		t.Fatalf("provider calls = %q, want no compensation calls", got)
	}
	if res.Applied == nil || len(*res.Applied) != 1 || (*res.Applied)[0].Address != "b" {
		t.Fatalf("applied = %v, want only b", res.Applied)
	}
	if got := stateAddresses(res); got != "b" {
		t.Fatalf("state = %q, want only b", got)
	}
}

func TestApplyRollbackSuccessAddsEmptyArrays(t *testing.T) {
	body := rollbackBody(`[`+resEntry("a", `[]`)+`]`, `[]`, `true`)
	provider := &recordingProvider{}
	res := doApply(t, HandlerWithProvider(provider), body, nil)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("status = %d errors=%v", res.status, res.Errors)
	}
	rolledBack, present := res.raw["rolledBack"].([]any)
	if !present || len(rolledBack) != 0 {
		t.Fatalf("rolledBack = %v, want present empty array", res.raw["rolledBack"])
	}
	rollbackErrors, present := res.raw["rollbackErrors"].([]any)
	if !present || len(rollbackErrors) != 0 {
		t.Fatalf("rollbackErrors = %v, want present empty array", res.raw["rollbackErrors"])
	}
	if got := providerCallsSummary(provider.calls); got != "create:a" {
		t.Fatalf("provider calls = %q, want only the forward create", got)
	}
	if res.Applied == nil || len(*res.Applied) != 1 {
		t.Fatalf("applied = %v, want the one create", res.Applied)
	}
}

func TestApplyRollbackCompensatesInReverseOrder(t *testing.T) {
	// Plan: delete old, create new, update keep; c is an untouched noop.
	// The update fails, so the create and the delete are compensated in
	// reverse: create new -> delete new, delete old -> create old.
	body := rollbackBody(
		`[`+resEntry("c", `[]`)+`,`+resEntry("keep", `["new"]`)+`,`+resEntry("new", `[]`)+`]`,
		`[`+
			stateEntry("old", "t", `{}`, `["keep"]`)+`,`+
			stateEntry("keep", "t", `{"v": 1}`, ``)+`,`+
			stateEntry("c", "t", `{}`, ``)+`]`,
		`true`)
	provider := &recordingProvider{failAt: 2, failErr: errors.New("update failed")}
	res := doApply(t, HandlerWithProvider(provider), body, nil)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("errors = %v, want exactly one provider_error", res.Errors)
	}
	e := res.Errors[0]
	if e.Code != codeProviderError || e.Message != "update failed" || e.Path != "/changes/2" {
		t.Fatalf("error = %+v, want provider_error at /changes/2", e)
	}

	wantCalls := "delete:old|create:new|update:keep|delete:new|create:old"
	if got := providerCallsSummary(provider.calls); got != wantCalls {
		t.Fatalf("provider calls = %q, want %q", got, wantCalls)
	}
	// The compensation of the delete recreates the before snapshot.
	recreate := provider.calls[4]
	if recreate.Action != "create" || recreate.After == nil || recreate.Before != nil {
		t.Fatalf("compensation of delete wrong: %+v", recreate)
	}
	if got := strings.Join(recreate.After.DependsOn, "|"); got != "keep" {
		t.Fatalf("recreated old dependsOn = %v, want [keep]", recreate.After.DependsOn)
	}

	// rolledBack lists the original changes in compensation order.
	rolledBack, ok := res.raw["rolledBack"].([]any)
	if !ok || len(rolledBack) != 2 {
		t.Fatalf("rolledBack = %v, want two entries", res.raw["rolledBack"])
	}
	if res.RolledBack == nil || rolledBackSummary(*res.RolledBack) != "create:new|delete:old" {
		t.Fatalf("rolledBack = %v, want create:new|delete:old", res.RolledBack)
	}
	rollbackErrors, ok := res.raw["rollbackErrors"].([]any)
	if !ok || len(rollbackErrors) != 0 {
		t.Fatalf("rollbackErrors = %v, want empty", res.raw["rollbackErrors"])
	}

	// Everything compensated: applied is empty and state is the normalized
	// prior state.
	if res.Applied == nil || len(*res.Applied) != 0 {
		t.Fatalf("applied = %v, want empty after full rollback", res.Applied)
	}
	if got := stateAddresses(res); got != "c|keep|old" {
		t.Fatalf("state = %q, want c|keep|old (the prior state)", got)
	}
	byAddress := map[string]stateResource{}
	for _, r := range res.State.Resources {
		byAddress[r.Address] = r
	}
	if byAddress["keep"].Properties["v"] != float64(1) {
		t.Fatalf("keep must be back to its prior properties, got %v", byAddress["keep"].Properties)
	}
	if got := strings.Join(byAddress["old"].DependsOn, "|"); got != "keep" {
		t.Fatalf("old dependsOn = %v, want [keep]", byAddress["old"].DependsOn)
	}
	if *res.Summary != (planSummary{Create: 1, Update: 1, Delete: 1, Noop: 1}) {
		t.Fatalf("summary = %+v, want the original plan summary", res.Summary)
	}
}

func TestApplyRollbackCompensatesUpdateBySwappingSnapshots(t *testing.T) {
	// update a, then create b which fails: a is compensated by an update
	// with before and after exchanged.
	body := rollbackBody(
		`[`+resEntry("a", `[]`)+`,`+resEntry("b", `[]`)+`]`,
		`[`+stateEntry("a", "t", `{"v": 1}`, `[]`)+`]`,
		`true`)
	provider := &recordingProvider{failAt: 1, failErr: errors.New("create failed")}
	res := doApply(t, HandlerWithProvider(provider), body, nil)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if got := providerCallsSummary(provider.calls); got != "update:a|create:b|update:a" {
		t.Fatalf("provider calls = %q, want update compensation last", got)
	}
	compensation := provider.calls[2]
	if compensation.Before == nil || compensation.After == nil {
		t.Fatalf("update compensation must carry both snapshots: %+v", compensation)
	}
	if compensation.Before.Properties["v"] != nil || compensation.After.Properties["v"] != json.Number("1") {
		t.Fatalf("snapshots not swapped: before=%v after=%v", compensation.Before, compensation.After)
	}
	if res.Applied == nil || len(*res.Applied) != 0 {
		t.Fatalf("applied = %v, want empty", res.Applied)
	}
	if got := stateAddresses(res); got != "a" {
		t.Fatalf("state = %q, want only a", got)
	}
	if res.State.Resources[0].Properties["v"] != float64(1) {
		t.Fatalf("a must be back to its prior properties, got %v", res.State.Resources[0].Properties)
	}
}

func TestApplyRollbackKeysAreFreshDistinctAndStablePerCompensation(t *testing.T) {
	// Two creates succeed, the third fails; both compensations run, one of
	// them only after a retryable failure.
	body := rollbackBody(
		`[`+resEntry("a", `[]`)+`,`+resEntry("b", `[]`)+`,`+resEntry("c", `[]`)+`]`,
		`[]`, `true`)
	provider := &compensationScriptProvider{failures: map[string]map[int]error{
		// c is created last and fails outright; b's compensation (a delete)
		// fails retryably on its first attempt.
		"create:c": {1: errors.New("create c failed")},
		"delete:b": {1: &retryableErr{msg: "transient", retryable: true}},
	}}
	res := doApply(t, HandlerWithProvider(provider), body, nil)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	calls := provider.calls
	if got := providerCallsSummary(calls); got != "create:a|create:b|create:c|delete:b|delete:b|delete:a" {
		t.Fatalf("calls = %q", got)
	}
	// Attempts: forward changes 1 each; b's compensation retries as 1 then 2.
	if got := attemptsFor(calls, "b"); fmt.Sprint(got) != "[1 1 2]" {
		t.Fatalf("b attempts = %v, want [1 1 2] (forward 1, compensation 1 and 2)", got)
	}
	// Every change and every compensation has its own non-empty key; retries
	// of one unit reuse that unit's key.
	keys := map[string]string{}
	for _, c := range calls {
		unit := c.Action + ":" + c.Address
		if c.IdempotencyKey == "" {
			t.Fatalf("empty idempotency key in %+v", c)
		}
		if existing, seen := keys[unit]; seen {
			if existing != c.IdempotencyKey {
				t.Fatalf("unit %s changed key across attempts: %q vs %q", unit, existing, c.IdempotencyKey)
			}
			continue
		}
		for other, key := range keys {
			if key == c.IdempotencyKey {
				t.Fatalf("units %s and %s share key %q", other, unit, key)
			}
		}
		keys[unit] = c.IdempotencyKey
	}
	if len(keys) != 5 {
		t.Fatalf("units = %v, want 5 (3 forward + 2 compensations)", keys)
	}
	if res.RolledBack == nil || rolledBackSummary(*res.RolledBack) != "create:b|create:a" {
		t.Fatalf("rolledBack = %v, want create:b|create:a", res.RolledBack)
	}
}

func TestApplyRollbackCompensationFailureKeepsResidualAndContinues(t *testing.T) {
	// a and b are created, c fails. b's compensation fails finally (three
	// retryable failures), a's compensation still runs and succeeds.
	body := rollbackBody(
		`[`+resEntry("a", `[]`)+`,`+resEntry("b", `[]`)+`,`+resEntry("c", `[]`)+`]`,
		`[]`, `true`)
	provider := &compensationScriptProvider{failures: map[string]map[int]error{
		"create:c": {1: errors.New("create c failed")},
		"delete:b": {
			1: &retryableErr{msg: "rb1", retryable: true},
			2: &retryableErr{msg: "rb2", retryable: true},
			3: &retryableErr{msg: "rb3", retryable: true},
		},
	}}
	res := doApply(t, HandlerWithProvider(provider), body, nil)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeProviderError ||
		res.Errors[0].Message != "create c failed" || res.Errors[0].Path != "/changes/2" {
		t.Fatalf("errors = %v, want the single forward provider_error", res.Errors)
	}
	calls := provider.calls
	if got := providerCallsSummary(calls); got != "create:a|create:b|create:c|delete:b|delete:b|delete:b|delete:a" {
		t.Fatalf("calls = %q, want b's compensation retried to exhaustion, then a's", got)
	}
	if got := attemptsFor(calls, "b"); fmt.Sprint(got) != "[1 1 2 3]" {
		t.Fatalf("b attempts = %v, want forward 1 plus compensation attempts 1, 2, 3", got)
	}

	// rollbackErrors carries the final compensation error at the original
	// change's index; a's earlier compensation still ran.
	if res.RollbackErrors == nil || len(*res.RollbackErrors) != 1 {
		t.Fatalf("rollbackErrors = %v, want one entry", res.RollbackErrors)
	}
	re := (*res.RollbackErrors)[0]
	if re.Code != codeRollbackError || re.Message != "rb3" || re.Path != "/rollbacks/1" {
		t.Fatalf("rollbackError = %+v, want rollback_error rb3 at /rollbacks/1", re)
	}
	if res.RolledBack == nil || rolledBackSummary(*res.RolledBack) != "create:a" {
		t.Fatalf("rolledBack = %v, want only create:a", res.RolledBack)
	}
	// b's compensation failed, so b stays in effect and in state.
	if res.Applied == nil || len(*res.Applied) != 1 || (*res.Applied)[0].Address != "b" ||
		(*res.Applied)[0].Action != "create" {
		t.Fatalf("applied = %v, want only the residual create:b", res.Applied)
	}
	if got := stateAddresses(res); got != "b" {
		t.Fatalf("state = %q, want only b", got)
	}
}

func TestApplyRollbackFirstChangeFailsCompensatesNothing(t *testing.T) {
	body := rollbackBody(`[`+resEntry("a", `[]`)+`]`, `[]`, `true`)
	provider := &recordingProvider{failAt: 0, failErr: errors.New("nope")}
	res := doApply(t, HandlerWithProvider(provider), body, nil)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if len(provider.calls) != 1 {
		t.Fatalf("provider calls = %v, want only the failed forward call", provider.calls)
	}
	rolledBack, ok := res.raw["rolledBack"].([]any)
	if !ok || len(rolledBack) != 0 {
		t.Fatalf("rolledBack = %v, want present empty array", res.raw["rolledBack"])
	}
	rollbackErrors, ok := res.raw["rollbackErrors"].([]any)
	if !ok || len(rollbackErrors) != 0 {
		t.Fatalf("rollbackErrors = %v, want present empty array", res.raw["rollbackErrors"])
	}
	if res.Applied == nil || len(*res.Applied) != 0 {
		t.Fatalf("applied = %v, want empty", res.Applied)
	}
	if got := stateAddresses(res); got != "" {
		t.Fatalf("state = %q, want empty", got)
	}
}

func TestApplyRollbackMultiProviderRoutesCompensationByTargetType(t *testing.T) {
	// u is updated from type a to type b, then v (type b) fails to create.
	// u's compensation is an update back to type a and must be routed to
	// provider a.
	pa := &recordingProvider{}
	// pb's first call is the forward update of u; its second, the create of
	// v, is the one that fails.
	pb := &recordingProvider{failAt: 1, failErr: errors.New("v failed")}
	handler := HandlerWithProviders(map[string]Provider{"a": pa, "b": pb})

	body := multiPlanBody(
		`[`+typedEntry("u", "b", `[]`)+`,`+typedEntry("v", "b", `[]`)+`]`,
		`[`+stateEntry("u", "a", `{}`, `[]`)+`]`)
	body = body[:len(body)-1] + `, "rollbackOnError": true}`
	res := doApply(t, handler, body, nil)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if got := providerCallsSummary(pb.calls); got != "update:u|create:v" {
		t.Fatalf("provider b calls = %q, want the forward update and failed create", got)
	}
	if got := providerCallsSummary(pa.calls); got != "update:u" {
		t.Fatalf("provider a calls = %q, want only the compensation update", got)
	}
	compensation := pa.calls[0]
	if compensation.Before == nil || compensation.Before.Type != "b" ||
		compensation.After == nil || compensation.After.Type != "a" {
		t.Fatalf("compensation snapshots wrong: %+v", compensation)
	}
	if res.RolledBack == nil || rolledBackSummary(*res.RolledBack) != "update:u" {
		t.Fatalf("rolledBack = %v, want update:u", res.RolledBack)
	}
	if got := stateAddresses(res); got != "u" {
		t.Fatalf("state = %q, want only u", got)
	}
	if res.State.Resources[0].Type != "a" {
		t.Fatalf("u must be back to type a, got %q", res.State.Resources[0].Type)
	}
}

func TestApplyRollbackPrecheckCoversCompensationRoutes(t *testing.T) {
	// u's update routes forward to type b (registered) but its compensation
	// targets type a (unregistered): the run aborts before any Provider call.
	pb := &recordingProvider{}
	handler := HandlerWithProviders(map[string]Provider{"b": pb})

	body := multiPlanBody(
		`[`+typedEntry("u", "b", `[]`)+`]`,
		`[`+stateEntry("u", "a", `{}`, `[]`)+`]`)
	body = body[:len(body)-1] + `, "rollbackOnError": true}`
	res := doApply(t, handler, body, nil)
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.status)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("errors = %v, want one", res.Errors)
	}
	e := res.Errors[0]
	if e.Code != codeProviderUnavailable || e.Path != "/changes/0" {
		t.Fatalf("error = %+v, want provider_unavailable at /changes/0", e)
	}
	if !strings.Contains(e.Message, `"a"`) {
		t.Fatalf("error message = %q, want it to name the unroutable type a", e.Message)
	}
	if len(pb.calls) != 0 {
		t.Fatalf("no Provider may be called, got %v", providerCallsSummary(pb.calls))
	}
	if _, present := res.raw["applied"]; present {
		t.Fatalf("503 response must not contain applied: %v", res.raw)
	}
}

func TestApplyRollbackUnavailableWithoutProvider(t *testing.T) {
	body := rollbackBody(`[`+resEntry("a", `[]`)+`]`, `[]`, `true`)
	res := doApply(t, Handler(), body, nil)
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeProviderUnavailable {
		t.Fatalf("errors = %v, want provider_unavailable", res.Errors)
	}
}

func TestPlanEndpointStillRejectsRollbackOnError(t *testing.T) {
	body := rollbackBody(`[`+resEntry("a", `[]`)+`]`, `[]`, `true`)
	req := httptest.NewRequest(http.MethodPost, "/v1/plans", strings.NewReader(body))
	rec := httptest.NewRecorder()
	HandlerWithProvider(&recordingProvider{}).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	var res planResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeInvalidPlanRequest ||
		res.Errors[0].Path != "/rollbackOnError" {
		t.Fatalf("errors = %v, want unknown-field invalid_plan_request at /rollbackOnError", res.Errors)
	}
}
