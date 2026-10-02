package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// recordingProvider is the injected Provider used by the apply tests. It
// records every request it receives in order and can be told to fail on a
// given change index.
type recordingProvider struct {
	calls   []ChangeRequest
	failAt  int
	failErr error
	ctx     context.Context
}

func (p *recordingProvider) Apply(req ChangeRequest) error {
	if p.ctx != nil && req.Context != p.ctx {
		return errors.New("request did not carry the inbound context")
	}
	idx := len(p.calls)
	p.calls = append(p.calls, req)
	if p.failErr != nil && idx == p.failAt {
		return p.failErr
	}
	return nil
}

type applyResult struct {
	status  int
	raw     map[string]any
	Valid   bool              `json:"valid"`
	Errors  []validationError `json:"errors"`
	Applied *[]planChange     `json:"applied"`
	Summary *planSummary      `json:"summary"`
	State   *struct {
		Resources []stateResource `json:"resources"`
	} `json:"state"`
}

func doApply(t *testing.T, handler http.Handler, body string, ctx context.Context) applyResult {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/apply", strings.NewReader(body))
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var res applyResult
	res.status = rec.Code
	if err := json.Unmarshal(rec.Body.Bytes(), &res.raw); err != nil {
		t.Fatalf("response is not JSON: %v (body %q)", err, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode typed response: %v (body %q)", err, rec.Body.String())
	}
	return res
}

func providerCallsSummary(calls []ChangeRequest) string {
	parts := make([]string, 0, len(calls))
	for _, c := range calls {
		parts = append(parts, c.Action+":"+c.Address)
	}
	return strings.Join(parts, "|")
}

func stateAddresses(res applyResult) string {
	if res.State == nil {
		return "<no state>"
	}
	parts := make([]string, 0, len(res.State.Resources))
	for _, r := range res.State.Resources {
		parts = append(parts, r.Address)
	}
	return strings.Join(parts, "|")
}

func TestApplyEmptyPlanSucceedsWithoutProvider(t *testing.T) {
	res := doApply(t, Handler(), planBody(`[]`, `[]`), nil)
	if res.status != http.StatusOK || !res.Valid || len(res.Errors) != 0 {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
	if res.Applied == nil || len(*res.Applied) != 0 {
		t.Fatalf("applied = %v, want empty array", res.Applied)
	}
	if res.Summary == nil || *res.Summary != (planSummary{}) {
		t.Fatalf("summary = %+v, want zero", res.Summary)
	}
	if res.State == nil || len(res.State.Resources) != 0 {
		t.Fatalf("state = %+v, want empty resources", res.State)
	}
}

func TestApplyNoopPlanSucceedsWithoutProviderAndKeepsState(t *testing.T) {
	// a and b are identical on both sides, so the plan is all noop.
	body := planBody(
		`[`+resEntry("a", `["b"]`)+`,`+resEntry("b", `[]`)+`]`,
		`[`+stateEntry("a", "t", `{}`, `["b"]`)+`,`+stateEntry("b", "t", `{}`, `[]`)+`]`)
	provider := &recordingProvider{}
	res := doApply(t, HandlerWithProvider(provider), body, nil)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
	if res.Applied == nil || len(*res.Applied) != 0 {
		t.Fatalf("applied = %v, want empty", res.Applied)
	}
	if len(provider.calls) != 0 {
		t.Fatalf("provider must not be called for noops, got %v", providerCallsSummary(provider.calls))
	}
	// Noop resources remain in state, address-sorted.
	if got := stateAddresses(res); got != "a|b" {
		t.Fatalf("state = %q, want a|b", got)
	}
	a := res.State.Resources[0]
	if got := strings.Join(a.DependsOn, "|"); got != "b" {
		t.Fatalf("a dependsOn = %v", a.DependsOn)
	}
}

func TestApplyChangesWithoutProviderIsUnavailable(t *testing.T) {
	body := planBody(`[`+resEntry("a", `[]`)+`]`, `[]`)
	res := doApply(t, Handler(), body, nil)
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.status)
	}
	if res.Valid || len(res.Errors) != 1 || res.Errors[0].Code != codeProviderUnavailable {
		t.Fatalf("unexpected payload: %+v", res)
	}
	if _, present := res.raw["applied"]; present {
		t.Fatalf("503 response must not contain applied: %v", res.raw)
	}
	if _, present := res.raw["state"]; present {
		t.Fatalf("503 response must not contain state: %v", res.raw)
	}
	if res.Summary == nil || res.Summary.Create != 1 {
		t.Fatalf("summary = %+v, want original plan summary", res.Summary)
	}
}

func TestApplySuccessExecutesInOrderAndBuildsState(t *testing.T) {
	// Plan: delete old (reverse state order), then create new, then update
	// keep; c is an untouched noop.
	body := planBody(
		`[`+resEntry("c", `[]`)+`,`+resEntry("keep", `["new"]`)+`,`+resEntry("new", `[]`)+`]`,
		`[`+
			stateEntry("old", "t", `{}`, `["keep"]`)+`,`+
			stateEntry("keep", "t", `{"v": 1}`, ``)+`,`+
			stateEntry("c", "t", `{}`, ``)+`]`)
	provider := &recordingProvider{}
	res := doApply(t, HandlerWithProvider(provider), body, nil)
	if res.status != http.StatusOK || !res.Valid || len(res.Errors) != 0 {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}

	wantCalls := "delete:old|create:new|update:keep"
	if got := providerCallsSummary(provider.calls); got != wantCalls {
		t.Fatalf("provider calls = %q, want %q", got, wantCalls)
	}
	if res.Applied == nil || len(*res.Applied) != 3 {
		t.Fatalf("applied = %v, want 3 entries", res.Applied)
	}
	if *res.Summary != (planSummary{Create: 1, Update: 1, Delete: 1, Noop: 1}) {
		t.Fatalf("summary = %+v", res.Summary)
	}

	// old removed; keep adopts after (no properties); new created; c kept.
	if got := stateAddresses(res); got != "c|keep|new" {
		t.Fatalf("state = %q, want c|keep|new", got)
	}
	byAddress := map[string]stateResource{}
	for _, r := range res.State.Resources {
		byAddress[r.Address] = r
	}
	keep := byAddress["keep"]
	if _, hasV := keep.Properties["v"]; hasV {
		t.Fatalf("keep must adopt after properties, got %v", keep.Properties)
	}
	if got := strings.Join(keep.DependsOn, "|"); got != "new" {
		t.Fatalf("keep dependsOn = %v, want [new]", keep.DependsOn)
	}
	if byAddress["new"].Type != "t" {
		t.Fatalf("new = %+v", byAddress["new"])
	}
}

func TestApplySingleChangeSnapshots(t *testing.T) {
	body := planBody(
		`[`+resEntry("u", `[]`)+`]`,
		`[`+stateEntry("u", "t", `{"v": 1}`, `[]`)+`]`)
	provider := &recordingProvider{}
	res := doApply(t, HandlerWithProvider(provider), body, nil)
	if res.status != http.StatusOK {
		t.Fatalf("status = %d errors=%v", res.status, res.Errors)
	}
	call := provider.calls[0]
	if call.Action != "update" || call.Address != "u" || call.Before == nil || call.After == nil {
		t.Fatalf("update request shape wrong: %+v", call)
	}
	if call.Before.Properties["v"] == nil || call.After.Properties["v"] != nil {
		t.Fatalf("update snapshots wrong: before=%v after=%v", call.Before, call.After)
	}
}

func TestApplyStateSortedByUnicodeCodePoint(t *testing.T) {
	// All creates; config topological order differs from output ordering.
	body := planBody(`[`+
		resEntry("中", `[]`)+`,`+
		resEntry("a", `[]`)+`,`+
		resEntry("b", `[]`)+`]`, `[]`)
	provider := &recordingProvider{}
	res := doApply(t, HandlerWithProvider(provider), body, nil)
	if res.status != http.StatusOK {
		t.Fatalf("status = %d errors=%v", res.status, res.Errors)
	}
	if got := stateAddresses(res); got != "a|b|中" {
		t.Fatalf("state = %q, want a|b|中 (Unicode code point order)", got)
	}
}

func TestApplyProviderFailureStopsAndReportsPartialState(t *testing.T) {
	// Creates execute in deterministic order b then a. Failing the second
	// must leave b applied and a never attempted.
	body := planBody(`[`+resEntry("a", `["b"]`)+`,`+resEntry("b", `[]`)+`]`, `[]`)
	provider := &recordingProvider{failAt: 1, failErr: errors.New("boom at a")}
	res := doApply(t, HandlerWithProvider(provider), body, nil)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if res.Valid {
		t.Fatal("valid must be false")
	}
	if len(res.Errors) != 1 {
		t.Fatalf("errors = %v, want exactly one", res.Errors)
	}
	e := res.Errors[0]
	if e.Code != codeProviderError || e.Message != "boom at a" || e.Path != "/changes/1" {
		t.Fatalf("error = %+v", e)
	}
	if got := providerCallsSummary(provider.calls); got != "create:b|create:a" {
		t.Fatalf("provider calls = %q, want both attempted (second failed)", got)
	}
	if res.Applied == nil || len(*res.Applied) != 1 || (*res.Applied)[0].Address != "b" {
		t.Fatalf("applied = %v, want only b", res.Applied)
	}
	if got := stateAddresses(res); got != "b" {
		t.Fatalf("state = %q, want only b", got)
	}
	if *res.Summary != (planSummary{Create: 2}) {
		t.Fatalf("summary = %+v, want the original plan summary", res.Summary)
	}
}

func TestApplyFailureAfterDeleteReflectsRemoval(t *testing.T) {
	// delete d succeeds, then create c fails: state must show d removed.
	body := planBody(
		`[`+resEntry("c", `[]`)+`]`,
		`[`+stateEntry("d", "t", `{}`, ``)+`]`)
	provider := &recordingProvider{failAt: 1, failErr: errors.New("cannot create")}
	res := doApply(t, HandlerWithProvider(provider), body, nil)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if got := stateAddresses(res); got != "" {
		t.Fatalf("state = %q, want d removed and c absent", got)
	}
}

func TestApplyCarriesRequestContext(t *testing.T) {
	type ctxKey string
	ctx := context.WithValue(context.Background(), ctxKey("k"), "v")
	provider := &recordingProvider{ctx: ctx}
	body := planBody(`[`+resEntry("a", `[]`)+`]`, `[]`)
	res := doApply(t, HandlerWithProvider(provider), body, ctx)
	if res.status != http.StatusOK {
		t.Fatalf("status = %d errors=%v", res.status, res.Errors)
	}
	if len(provider.calls) != 1 || provider.calls[0].Context == nil {
		t.Fatalf("provider did not receive the request context: %+v", provider.calls)
	}
	if got := provider.calls[0].Context.Value(ctxKey("k")); got != "v" {
		t.Fatalf("context value = %v", got)
	}
}

func TestApplyInvalidJSONIs400AndDoesNotCallProvider(t *testing.T) {
	provider := &recordingProvider{}
	for _, body := range []string{"", `{`, planBody(`[]`, `[]`) + ` {}`} {
		res := doApply(t, HandlerWithProvider(provider), body, nil)
		if res.status != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", res.status)
		}
		if res.Valid || len(res.Errors) != 1 || res.Errors[0].Code != codeInvalidJSON {
			t.Fatalf("payload = %+v", res)
		}
	}
	if len(provider.calls) != 0 {
		t.Fatalf("provider must not be called, got %v", provider.calls)
	}
}

func TestApplyValidationErrorsAre422AndDoNotCallProvider(t *testing.T) {
	provider := &recordingProvider{}
	cases := map[string]string{
		"envelope": `{}`,
		"config":   `{"configuration": {"resourceTypes": [], "resources": [{"address":"a","type":"x","properties":{}}]}, "priorState": {"resources": []}}`,
		"state":    `{"configuration": {"resourceTypes": [], "resources": []}, "priorState": {"resources": [{}]}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			res := doApply(t, HandlerWithProvider(provider), body, nil)
			if res.status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422", res.status)
			}
			if res.Valid || len(res.Errors) == 0 {
				t.Fatalf("payload = %+v", res)
			}
			if _, present := res.raw["applied"]; present {
				t.Fatalf("422 response must not contain applied: %v", res.raw)
			}
			if _, present := res.raw["state"]; present {
				t.Fatalf("422 response must not contain state: %v", res.raw)
			}
		})
	}
	if len(provider.calls) != 0 {
		t.Fatalf("provider must not be called, got %v", provider.calls)
	}
}

func TestApplyRejectsOtherMethods(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		HandlerWithProvider(&recordingProvider{}).ServeHTTP(rec, httptest.NewRequest(method, "/v1/apply", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
			t.Fatalf("%s: Allow = %q, want POST", method, allow)
		}
		var payload map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("%s: body is not JSON: %v", method, err)
		}
		if code, _ := payload["error"].(map[string]any)["code"].(string); code != "method_not_allowed" {
			t.Fatalf("%s: body = %v", method, payload)
		}
	}
}

func TestApplyDoesNotPersistAcrossRequests(t *testing.T) {
	provider := &recordingProvider{}
	handler := HandlerWithProvider(provider)
	// First run creates a against empty state.
	first := doApply(t, handler, planBody(`[`+resEntry("a", `[]`)+`]`, `[]`), nil)
	if first.status != http.StatusOK {
		t.Fatalf("first status = %d", first.status)
	}
	// Second run with empty state must still plan a create and see empty
	// prior state, proving the first run was not remembered.
	second := doApply(t, handler, planBody(`[`+resEntry("a", `[]`)+`]`, `[]`), nil)
	if second.status != http.StatusOK {
		t.Fatalf("second status = %d", second.status)
	}
	if len(provider.calls) != 2 || providerCallsSummary(provider.calls) != "create:a|create:a" {
		t.Fatalf("calls = %v, want two independent creates", providerCallsSummary(provider.calls))
	}
	if got := stateAddresses(second); got != "a" {
		t.Fatalf("second state = %q, want only a (not accumulated)", got)
	}
}

func TestApplyKeepsOtherEndpointsWorking(t *testing.T) {
	handler := HandlerWithProvider(&recordingProvider{})

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("healthz status = %d", health.Code)
	}

	// The read-only plan endpoint remains available through the provider
	// handler and does not execute anything.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/plans",
		strings.NewReader(planBody(`[`+resEntry("a", `[]`)+`]`, `[]`))))
	if rec.Code != http.StatusOK {
		t.Fatalf("plans status = %d", rec.Code)
	}
	var plan planResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &plan); err != nil || !plan.Valid || len(plan.Changes) != 1 {
		t.Fatalf("plans response wrong: %v %s", err, rec.Body.String())
	}
}
