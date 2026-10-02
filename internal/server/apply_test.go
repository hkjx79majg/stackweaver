package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// recordingProvider records every applied request and optionally fails at a
// fixed change index with a fixed message.
type recordingProvider struct {
	calls   []ChangeRequest
	failAt  int
	failMsg string
	ctxOK   bool
}

func (p *recordingProvider) Apply(ctx context.Context, req ChangeRequest) error {
	if ctx == nil {
		p.ctxOK = false
	} else {
		p.ctxOK = true
	}
	p.calls = append(p.calls, req)
	if p.failAt >= 0 && len(p.calls)-1 == p.failAt {
		return errors.New(p.failMsg)
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
		Resources []map[string]any `json:"resources"`
	} `json:"state"`
}

func postApply(t *testing.T, provider Provider, body string) applyResult {
	t.Helper()
	rec := httptest.NewRecorder()
	HandlerWithProvider(provider).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apply", strings.NewReader(body)))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var res applyResult
	res.status = rec.Code
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("response is not JSON: %v (body %q)", err, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res.raw); err != nil {
		t.Fatalf("raw decode failed: %v", err)
	}
	return res
}

func stateResourceAddresses(res applyResult) []string {
	if res.State == nil {
		return nil
	}
	got := make([]string, 0, len(res.State.Resources))
	for _, entry := range res.State.Resources {
		got = append(got, entry["address"].(string))
	}
	return got
}

func TestApplyEmptyPlanSucceedsWithoutProvider(t *testing.T) {
	res := postApply(t, nil, planBody(`[]`, `[]`))
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
		t.Fatalf("state = %+v, want empty resources array", res.State)
	}
}

func TestApplyNoopPlanDoesNotCallProvider(t *testing.T) {
	config := `[` + resEntry("a", `[]`) + `]`
	state := `[` + stateEntry("a", "t", `{}`, `[]`) + `]`
	provider := &recordingProvider{failAt: -1}
	res := postApply(t, provider, planBody(config, state))
	if res.status != http.StatusOK {
		t.Fatalf("status=%d errors=%v", res.status, res.Errors)
	}
	if len(provider.calls) != 0 {
		t.Fatalf("provider called %d times for noop plan, want 0: %+v", len(provider.calls), provider.calls)
	}
	if got := stateResourceAddresses(res); len(got) != 1 || got[0] != "a" {
		t.Fatalf("state = %v, want prior resource a kept", got)
	}
	if *res.Summary != (planSummary{Noop: 1}) {
		t.Fatalf("summary = %+v", res.Summary)
	}
}

func TestApplyChangesWithoutProviderIs503(t *testing.T) {
	body := planBody(`[`+resEntry("a", `[]`)+`]`, `[]`)
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/apply", strings.NewReader(body)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if valid, _ := payload["valid"].(bool); valid {
		t.Fatalf("valid must be false: %v", payload)
	}
	errs, _ := payload["errors"].([]any)
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want one", payload["errors"])
	}
	first := errs[0].(map[string]any)
	if first["code"] != "provider_unavailable" {
		t.Fatalf("error = %v, want provider_unavailable", first)
	}
	if _, present := payload["applied"]; present {
		t.Fatalf("503 must not contain applied: %v", payload)
	}
	if _, present := payload["state"]; present {
		t.Fatalf("503 must not contain state: %v", payload)
	}
}

func TestApplySuccessExecutesInPlanOrderAndBuildsState(t *testing.T) {
	// Deletes precede creates/updates: delete old, update a, create b (b
	// depends on a). "same" is a noop (identical on both sides) and must be
	// kept verbatim.
	config := `[` +
		resEntry("b", `["a"]`) + `,` +
		`{"address":"a","type":"t","properties":{"n":1}},` +
		`{"address":"same","type":"t","properties":{"k":"v"},"dependsOn":[]}` + `]`
	state := `[` +
		stateEntry("old", "t", `{}`, `["a"]`) + `,` +
		stateEntry("a", "t", `{"n":0}`, `[]`) + `,` +
		stateEntry("same", "t", `{"k":"v"}`, `[]`) + `]`
	provider := &recordingProvider{failAt: -1}
	res := postApply(t, provider, planBody(config, state))
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}

	wantCalls := "delete:old|update:a|create:b"
	if got := changeSummary(changeRequestsToPlan(provider.calls)); got != wantCalls {
		t.Fatalf("provider calls = %v, want %v", got, wantCalls)
	}
	if !provider.ctxOK {
		t.Fatal("provider did not receive a non-nil context")
	}
	if res.Applied == nil || len(*res.Applied) != 3 {
		t.Fatalf("applied = %v, want the three changes", res.Applied)
	}
	if *res.Summary != (planSummary{Create: 1, Update: 1, Delete: 1, Noop: 1}) {
		t.Fatalf("summary = %+v", res.Summary)
	}

	// State: old removed, a adopts after, b created, same kept; sorted by
	// address: a, b, same.
	got := stateResourceAddresses(res)
	if strings.Join(got, "|") != "a|b|same" {
		t.Fatalf("state addresses = %v, want a|b|same", got)
	}
	byAddress := map[string]map[string]any{}
	for _, entry := range res.State.Resources {
		byAddress[entry["address"].(string)] = entry
	}
	a := byAddress["a"]
	if a["type"] != "t" {
		t.Fatalf("a type = %v", a["type"])
	}
	props, _ := a["properties"].(map[string]any)
	if n, _ := props["n"].(float64); n != 1 {
		t.Fatalf("a properties = %v, want after snapshot n=1", a["properties"])
	}
	b := byAddress["b"]
	deps, _ := b["dependsOn"].([]any)
	if len(deps) != 1 || deps[0] != "a" {
		t.Fatalf("b dependsOn = %v, want [a]", b["dependsOn"])
	}
	same := byAddress["same"]
	sameProps, _ := same["properties"].(map[string]any)
	if sameProps["k"] != "v" {
		t.Fatalf("noop resource must keep prior form: %v", same)
	}
	if _, present := same["dependsOn"]; !present {
		t.Fatalf("noop resource must keep prior dependsOn field: %v", same)
	}
}

func changeRequestsToPlan(reqs []ChangeRequest) []planChange {
	out := make([]planChange, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, planChange{Address: r.Address, Action: r.Action})
	}
	return out
}

func TestApplySnapshotsCarryCorrectSides(t *testing.T) {
	// update carries both before and after; create only after.
	config := `[` +
		`{"address":"a","type":"t","properties":{"n":1}},` +
		resEntry("b", `[]`) + `]`
	state := `[` + stateEntry("a", "t", `{"n":0}`, `[]`) + `]`
	provider := &recordingProvider{failAt: -1}
	res := postApply(t, provider, planBody(config, state))
	if res.status != http.StatusOK {
		t.Fatalf("status=%d errors=%v", res.status, res.Errors)
	}
	if len(provider.calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(provider.calls))
	}
	update := provider.calls[0]
	if update.Action != "update" || update.Before == nil || update.After == nil {
		t.Fatalf("update request = %+v", update)
	}
	if update.Before.Properties["n"].(json.Number).String() != "0" {
		t.Fatalf("before = %+v", update.Before)
	}
	if update.After.Properties["n"].(json.Number).String() != "1" {
		t.Fatalf("after = %+v", update.After)
	}
	create := provider.calls[1]
	if create.Action != "create" || create.Before != nil || create.After == nil {
		t.Fatalf("create request = %+v", create)
	}
}

func TestApplyProviderFailureStopsAndKeepsPartialState(t *testing.T) {
	// Plan: delete old, create a, create b. Fail at index 1 (create a).
	// "keep" is a noop present on both sides and must survive.
	config := `[` + resEntry("a", `[]`) + `,` + resEntry("b", `[]`) + `,` +
		`{"address":"keep","type":"t","properties":{},"dependsOn":[]}` + `]`
	state := `[` +
		stateEntry("old", "t", `{}`, `[]`) + `,` +
		stateEntry("keep", "t", `{}`, `[]`) + `]`
	provider := &recordingProvider{failAt: 1, failMsg: "boom at a"}
	res := postApply(t, provider, planBody(config, state))
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
	if e.Code != "provider_error" || e.Message != "boom at a" || e.Path != "/changes/1" {
		t.Fatalf("error = %+v, want provider_error /changes/1 with provider text", e)
	}
	// Later change b is never attempted; no rollback of the delete.
	if got := changeSummary(changeRequestsToPlan(provider.calls)); got != "delete:old|create:a" {
		t.Fatalf("calls = %v, must stop after failing item", got)
	}
	if res.Applied == nil || len(*res.Applied) != 1 || (*res.Applied)[0].Address != "old" {
		t.Fatalf("applied = %v, want only the succeeded delete:old", res.Applied)
	}
	// Original full-plan summary is returned unchanged.
	if *res.Summary != (planSummary{Create: 2, Delete: 1, Noop: 1}) {
		t.Fatalf("summary = %+v, want original plan summary", res.Summary)
	}
	// Partial state: old deleted, keep remains, a/b not present (a failed).
	got := stateResourceAddresses(res)
	if strings.Join(got, "|") != "keep" {
		t.Fatalf("partial state = %v, want only keep", got)
	}
}

func TestApplyIsStatelessAcrossRequests(t *testing.T) {
	provider := &recordingProvider{failAt: -1}
	body := planBody(`[`+resEntry("a", `[]`)+`]`, `[]`)
	for i := 0; i < 2; i++ {
		res := postApply(t, provider, body)
		if res.status != http.StatusOK {
			t.Fatalf("request %d status=%d errors=%v", i, res.status, res.Errors)
		}
		// Each request starts from the submitted priorState (empty): a is
		// created every time, proving the service stores nothing.
		got := stateResourceAddresses(res)
		if len(got) != 1 || got[0] != "a" {
			t.Fatalf("request %d state = %v, want [a]", i, got)
		}
	}
	if len(provider.calls) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(provider.calls))
	}
}

func TestApplyRejectsOtherMethods(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		HandlerWithProvider(&recordingProvider{failAt: -1}).ServeHTTP(rec, httptest.NewRequest(method, "/v1/apply", nil))
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

func TestApplyMalformedJSON(t *testing.T) {
	for name, body := range map[string]string{
		"empty":  "",
		"broken": `{`,
	} {
		t.Run(name, func(t *testing.T) {
			provider := &recordingProvider{failAt: -1}
			res := postApply(t, provider, body)
			if res.status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", res.status)
			}
			if res.Valid || len(res.Errors) != 1 || res.Errors[0].Code != "invalid_json" {
				t.Fatalf("payload = %+v", res)
			}
			if len(provider.calls) != 0 {
				t.Fatal("provider must not be called for invalid JSON")
			}
		})
	}
}

func TestApplyValidationErrorsDoNotCallProvider(t *testing.T) {
	cases := map[string]string{
		"envelope": `{}`,
		"config":   `{"configuration": {"resourceTypes": [], "resources": [{"address":"a","type":"x","properties":{}}]}, "priorState": {"resources": []}}`,
		"state":    `{"configuration": {"resourceTypes": [], "resources": []}, "priorState": {"resources": [{}]}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			provider := &recordingProvider{failAt: -1}
			res := postApply(t, provider, body)
			if res.status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (errors %v)", res.status, res.Errors)
			}
			if res.Valid || len(res.Errors) == 0 {
				t.Fatalf("payload = %+v", res)
			}
			if len(provider.calls) != 0 {
				t.Fatal("provider must not be called for validation failures")
			}
			if _, present := res.raw["applied"]; present {
				t.Fatalf("422 must not contain applied: %v", res.raw)
			}
			if _, present := res.raw["state"]; present {
				t.Fatalf("422 must not contain state: %v", res.raw)
			}
		})
	}
}

func TestApplyValidationErrorsStaySortedByPath(t *testing.T) {
	body := `{
	  "configuration": {"resourceTypes": [], "resources": [{"address": "a", "type": "ghost", "properties": {}}]},
	  "priorState": {"resources": [{"address": "a", "type": "t"}, {"address": "b", "type": "t", "properties": {}}]}
	}`
	res := postApply(t, &recordingProvider{failAt: -1}, body)
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", res.status)
	}
	want := []string{
		"unknown_resource_type@/configuration/resources/0/type",
		"invalid_plan_request@/priorState/resources/0",
	}
	got := make([]string, 0, len(res.Errors))
	for _, e := range res.Errors {
		got = append(got, e.Code+"@"+e.Path)
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestApplyProviderInterfaceSatisfied(t *testing.T) {
	var _ Provider = (*recordingProvider)(nil)
	var _ Provider = ProviderFunc(nil)
}

// ProviderFunc adapts a function into a Provider.
type ProviderFunc func(ctx context.Context, req ChangeRequest) error

func (f ProviderFunc) Apply(ctx context.Context, req ChangeRequest) error { return f(ctx, req) }

func TestApplyProviderFuncReceivesRequest(t *testing.T) {
	var seen []string
	fn := ProviderFunc(func(_ context.Context, req ChangeRequest) error {
		seen = append(seen, fmt.Sprintf("%s:%s", req.Action, req.Address))
		return nil
	})
	res := postApply(t, fn, planBody(`[`+resEntry("a", `[]`)+`]`, `[]`))
	if res.status != http.StatusOK {
		t.Fatalf("status=%d errors=%v", res.status, res.Errors)
	}
	if strings.Join(seen, "|") != "create:a" {
		t.Fatalf("seen = %v", seen)
	}
}
