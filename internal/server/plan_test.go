package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type planResult struct {
	status  int
	Valid   bool              `json:"valid"`
	Errors  []validationError `json:"errors"`
	Changes *[]planChange     `json:"changes"`
	Summary *planSummary      `json:"summary"`
}

func postPlan(t *testing.T, body string) planResult {
	t.Helper()
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/plans", strings.NewReader(body)))
	var res planResult
	res.status = rec.Code
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("response is not JSON: %v (body %q)", err, rec.Body.String())
	}
	return res
}

func planErrCodes(res planResult) []string {
	codes := make([]string, 0, len(res.Errors))
	for _, e := range res.Errors {
		codes = append(codes, e.Code+"@"+e.Path)
	}
	return codes
}

// planBody wraps a configuration resources fragment and a prior state
// resources fragment in a valid plan request envelope.
func planBody(cfgResources, stateResources string) string {
	return `{
	  "configuration": {
	    "resourceTypes": [{"name": "t", "schema": {"type": "object", "additionalProperties": true}}],
	    "resources": ` + cfgResources + `
	  },
	  "priorState": {"resources": ` + stateResources + `}
	}`
}

func stateEntry(address, typ, properties, dependsOn string) string {
	entry := `{"address": "` + address + `", "type": "` + typ + `", "properties": ` + properties
	if dependsOn != "" {
		entry += `, "dependsOn": ` + dependsOn
	}
	return entry + `}`
}

func cfgEntry(address, typ, properties, dependsOn string) string {
	entry := `{"address": "` + address + `", "type": "` + typ + `", "properties": ` + properties
	if dependsOn != "" {
		entry += `, "dependsOn": ` + dependsOn
	}
	return entry + `}`
}

func changeSummary(changes []planChange) string {
	parts := make([]string, 0, len(changes))
	for _, c := range changes {
		parts = append(parts, c.Action+":"+c.Address)
	}
	return strings.Join(parts, "|")
}

func TestPlanEmpty(t *testing.T) {
	res := postPlan(t, planBody(`[]`, `[]`))
	if res.status != http.StatusOK || !res.Valid || len(res.Errors) != 0 {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
	if res.Changes == nil || len(*res.Changes) != 0 {
		t.Fatalf("changes = %v, want []", res.Changes)
	}
	if res.Summary == nil || *res.Summary != (planSummary{}) {
		t.Fatalf("summary = %+v, want all zero", res.Summary)
	}
}

func TestPlanCreateOnly(t *testing.T) {
	body := planBody(`[`+cfgEntry("b", "t", `{"x": 1}`, `["a"]`)+`,`+cfgEntry("a", "t", `{}`, ``)+`]`, `[]`)
	res := postPlan(t, body)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d errors=%v", res.status, res.Errors)
	}
	// Configuration topological order: a before b.
	if got := changeSummary(*res.Changes); got != "create:a|create:b" {
		t.Fatalf("changes = %v", got)
	}
	if *res.Summary != (planSummary{Create: 2}) {
		t.Fatalf("summary = %+v", *res.Summary)
	}
	c := (*res.Changes)[0]
	if c.Before != nil || c.After == nil {
		t.Fatalf("create must carry only after: %+v", c)
	}
	if c.After.Type != "t" || len(c.After.DependsOn) != 0 {
		t.Fatalf("after = %+v", c.After)
	}
	b := (*res.Changes)[1]
	if len(b.After.DependsOn) != 1 || b.After.DependsOn[0] != "a" {
		t.Fatalf("after.dependsOn = %v", b.After.DependsOn)
	}
}

func TestPlanDeleteOnlyReverseTopological(t *testing.T) {
	// State: a depends on b, b depends on c. Deletes must list dependents
	// before their dependencies: a, b, c.
	body := planBody(`[]`, `[`+
		stateEntry("a", "t", `{}`, `["b"]`)+`,`+
		stateEntry("b", "t", `{}`, `["c"]`)+`,`+
		stateEntry("c", "gone.type", `{}`, ``)+`]`)

	res := postPlan(t, body)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d errors=%v", res.status, res.Errors)
	}
	if got := changeSummary(*res.Changes); got != "delete:a|delete:b|delete:c" {
		t.Fatalf("changes = %v", got)
	}
	if *res.Summary != (planSummary{Delete: 3}) {
		t.Fatalf("summary = %+v", *res.Summary)
	}
	c := (*res.Changes)[0]
	if c.After != nil || c.Before == nil {
		t.Fatalf("delete must carry only before: %+v", c)
	}
	// State types unknown to the configuration are kept as-is.
	last := (*res.Changes)[2]
	if last.Before.Type != "gone.type" {
		t.Fatalf("before.type = %q", last.Before.Type)
	}
}

func TestPlanUpdateAndNoop(t *testing.T) {
	body := planBody(
		`[`+
			cfgEntry("same", "t", `{"n": 1, "obj": {"a": 1, "b": [1, 2]}}`, `["dep"]`)+`,`+
			cfgEntry("dep", "t", `{}`, ``)+`,`+
			cfgEntry("changed", "t", `{"n": 2}`, ``)+
			`]`,
		`[`+
			// Key order, number literal form, and dependsOn order differ;
			// semantically identical, so this is a noop.
			stateEntry("same", "t", `{"obj": {"b": [1, 2], "a": 1.0}, "n": 1.0}`, `["dep"]`)+`,`+
			stateEntry("dep", "t", `{}`, ``)+`,`+
			stateEntry("changed", "t", `{"n": 1}`, ``)+
			`]`)
	res := postPlan(t, body)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d errors=%v", res.status, res.Errors)
	}
	if got := changeSummary(*res.Changes); got != "update:changed" {
		t.Fatalf("changes = %v", got)
	}
	if *res.Summary != (planSummary{Update: 1, Noop: 2}) {
		t.Fatalf("summary = %+v", *res.Summary)
	}
	c := (*res.Changes)[0]
	if c.Before == nil || c.After == nil {
		t.Fatalf("update must carry before and after: %+v", c)
	}
}

func TestPlanUpdateOnTypeAndDependsOnChange(t *testing.T) {
	body := planBody(
		`[`+cfgEntry("a", "t", `{}`, `["b"]`)+`,`+cfgEntry("b", "t", `{}`, ``)+`]`,
		`[`+stateEntry("a", "other", `{}`, `[]`)+`,`+stateEntry("b", "t", `{}`, ``)+`]`)
	res := postPlan(t, body)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d errors=%v", res.status, res.Errors)
	}
	if got := changeSummary(*res.Changes); got != "update:a" {
		t.Fatalf("changes = %v", got)
	}
	c := (*res.Changes)[0]
	if c.Before.Type != "other" || c.After.Type != "t" {
		t.Fatalf("types: before=%q after=%q", c.Before.Type, c.After.Type)
	}
	if len(c.Before.DependsOn) != 0 || len(c.After.DependsOn) != 1 || c.After.DependsOn[0] != "b" {
		t.Fatalf("dependsOn: before=%v after=%v", c.Before.DependsOn, c.After.DependsOn)
	}
}

func TestPlanArrayOrderIsSignificant(t *testing.T) {
	body := planBody(
		`[`+cfgEntry("a", "t", `{"xs": [1, 2]}`, ``)+`]`,
		`[`+stateEntry("a", "t", `{"xs": [2, 1]}`, ``)+`]`)
	res := postPlan(t, body)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d errors=%v", res.status, res.Errors)
	}
	if *res.Summary != (planSummary{Update: 1}) {
		t.Fatalf("summary = %+v, want one update", *res.Summary)
	}
}

func TestPlanChangeOrderingDeletesFirst(t *testing.T) {
	// State: old depends on keep. Desired: keep (unchanged) and new.
	// Deletes come first in reverse state topological order, then creates in
	// configuration topological order.
	body := planBody(
		`[`+cfgEntry("new", "t", `{}`, `["keep"]`)+`,`+cfgEntry("keep", "t", `{}`, ``)+`]`,
		`[`+stateEntry("old", "t", `{}`, `["keep"]`)+`,`+stateEntry("keep", "t", `{}`, ``)+`]`)
	res := postPlan(t, body)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d errors=%v", res.status, res.Errors)
	}
	if got := changeSummary(*res.Changes); got != "delete:old|create:new" {
		t.Fatalf("changes = %v", got)
	}
	if *res.Summary != (planSummary{Create: 1, Delete: 1, Noop: 1}) {
		t.Fatalf("summary = %+v", *res.Summary)
	}
}

func TestPlanSnapshotDependsOnNormalized(t *testing.T) {
	body := planBody(
		`[`+cfgEntry("a", "t", `{}`, `["c", "b"]`)+`,`+cfgEntry("b", "t", `{}`, ``)+`,`+cfgEntry("c", "t", `{}`, ``)+`]`,
		`[]`)
	res := postPlan(t, body)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d errors=%v", res.status, res.Errors)
	}
	var created *planChange
	for i := range *res.Changes {
		if (*res.Changes)[i].Address == "a" {
			created = &(*res.Changes)[i]
		}
	}
	if created == nil {
		t.Fatalf("no create for a: %v", changeSummary(*res.Changes))
	}
	got := strings.Join(created.After.DependsOn, ",")
	if got != "b,c" {
		t.Fatalf("dependsOn = %v, want [b c]", created.After.DependsOn)
	}
}

func TestPlanMalformedJSON(t *testing.T) {
	for name, body := range map[string]string{
		"empty":         "",
		"broken":        `{`,
		"trailingValue": planBody(`[]`, `[]`) + ` {}`,
	} {
		t.Run(name, func(t *testing.T) {
			res := postPlan(t, body)
			if res.status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", res.status)
			}
			if res.Valid || len(res.Errors) != 1 || res.Errors[0].Code != "invalid_json" {
				t.Fatalf("unexpected payload: %+v", res)
			}
		})
	}
}

func TestPlanRejectsOtherMethods(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, httptest.NewRequest(method, "/v1/plans", nil))
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

func TestPlanRequestShapeErrors(t *testing.T) {
	cases := map[string]struct {
		body string
		want []string
	}{
		"nonObjectRoot": {
			body: `[]`,
			want: []string{"invalid_plan_request@"},
		},
		"unknownRootField": {
			body: `{"configuration": {}, "priorState": {"resources": []}, "extra": 1}`,
			want: []string{
				"invalid_document@/configuration",
				"invalid_document@/configuration",
				"invalid_plan_request@/extra",
			},
		},
		"missingFields": {
			body: `{}`,
			want: []string{
				"invalid_plan_request@",
				"invalid_plan_request@",
			},
		},
		"stateNotObject": {
			body: `{"configuration": {"resourceTypes": [], "resources": []}, "priorState": []}`,
			want: []string{"invalid_plan_request@/priorState"},
		},
		"stateResourcesNotArray": {
			body: `{"configuration": {"resourceTypes": [], "resources": []}, "priorState": {"resources": {}}}`,
			want: []string{"invalid_plan_request@/priorState/resources"},
		},
		"stateEntryShape": {
			body: `{"configuration": {"resourceTypes": [], "resources": []}, "priorState": {"resources": [
			  5,
			  {"type": "t", "properties": {}},
			  {"address": "", "type": 3, "properties": []},
			  {"address": "ok", "type": "t"}
			]}}`,
			want: []string{
				"invalid_plan_request@/priorState/resources/0",
				"invalid_plan_request@/priorState/resources/1",
				"invalid_plan_request@/priorState/resources/2/address",
				"invalid_plan_request@/priorState/resources/2/properties",
				"invalid_plan_request@/priorState/resources/2/type",
				"invalid_plan_request@/priorState/resources/3",
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			res := postPlan(t, tc.body)
			if res.status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (errors %v)", res.status, res.Errors)
			}
			if res.Valid {
				t.Fatal("valid must be false")
			}
			got := planErrCodes(res)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPlanStateDuplicateAddress(t *testing.T) {
	body := planBody(`[]`, `[`+
		stateEntry("a", "t", `{}`, ``)+`,`+
		stateEntry("a", "t", `{}`, ``)+`]`)
	res := postPlan(t, body)
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", res.status)
	}
	want := []string{"duplicate_identifier@/priorState/resources/1/address"}
	if got := planErrCodes(res); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestPlanStateDependencyErrors(t *testing.T) {
	body := planBody(`[]`, `[`+
		stateEntry("a", "t", `{}`, `["a"]`)+`,`+
		stateEntry("b", "t", `{}`, `["ghost", "b", "b"]`)+`,`+
		stateEntry("c", "t", `{}`, `{}`)+`,`+
		stateEntry("d", "t", `{}`, `[3]`)+`]`)
	res := postPlan(t, body)
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", res.status)
	}
	want := []string{
		"self_dependency@/priorState/resources/0/dependsOn/0",
		"unknown_dependency@/priorState/resources/1/dependsOn/0",
		"self_dependency@/priorState/resources/1/dependsOn/1",
		"duplicate_dependency@/priorState/resources/1/dependsOn/2",
		"invalid_plan_request@/priorState/resources/2/dependsOn",
		"invalid_plan_request@/priorState/resources/3/dependsOn/0",
	}
	if got := planErrCodes(res); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestPlanStateDependencyCycle(t *testing.T) {
	body := planBody(`[]`, `[`+
		stateEntry("a", "t", `{}`, `["b"]`)+`,`+
		stateEntry("b", "t", `{}`, `["a"]`)+`]`)
	res := postPlan(t, body)
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", res.status)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("errors = %v", res.Errors)
	}
	e := res.Errors[0]
	if e.Code != "dependency_cycle" || e.Path != "/priorState/resources" {
		t.Fatalf("unexpected error: %+v", e)
	}
	if e.Message != "dependency cycle: a, b" {
		t.Fatalf("message = %q", e.Message)
	}
}

func TestPlanConfigurationErrorsPrefixed(t *testing.T) {
	body := `{
	  "configuration": {"resourceTypes": [], "resources": [
	    {"address": "a", "type": "ghost", "properties": {}}
	  ]},
	  "priorState": {"resources": []}
	}`
	res := postPlan(t, body)
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", res.status)
	}
	want := []string{"unknown_resource_type@/configuration/resources/0/type"}
	if got := planErrCodes(res); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestPlanConfigurationCyclePrefixed(t *testing.T) {
	body := planBody(
		`[`+cfgEntry("a", "t", `{}`, `["b"]`)+`,`+cfgEntry("b", "t", `{}`, `["a"]`)+`]`,
		`[]`)
	res := postPlan(t, body)
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != "dependency_cycle" || res.Errors[0].Path != "/configuration/resources" {
		t.Fatalf("errors = %v", res.Errors)
	}
}

func TestPlanErrorsSortedAcrossSides(t *testing.T) {
	// Configuration findings sort under /configuration, state findings under
	// /priorState; within each, by path then code.
	body := `{
	  "configuration": {"resourceTypes": [], "resources": [
	    {"address": "a", "type": "ghost", "properties": {}}
	  ]},
	  "priorState": {"resources": [
	    {"address": "x", "type": "t"},
	    {"address": "x", "type": "t", "properties": {}}
	  ]}
	}`
	res := postPlan(t, body)
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", res.status)
	}
	want := []string{
		"unknown_resource_type@/configuration/resources/0/type",
		"invalid_plan_request@/priorState/resources/0",
		"duplicate_identifier@/priorState/resources/1/address",
	}
	if got := planErrCodes(res); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestPlanFailureResponsesOmitChangesAndSummary(t *testing.T) {
	cases := map[string]string{
		"invalidJSON": `{`,
		"shape":       `{}`,
		"config":      `{"configuration": {"resourceTypes": [], "resources": [{"address":"a","type":"x","properties":{}}]}, "priorState": {"resources": []}}`,
		"state":       planBody(`[]`, `[`+stateEntry("a", "t", `{}`, `["ghost"]`)+`]`),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/plans", strings.NewReader(body)))
			var payload map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatalf("body is not JSON: %v", err)
			}
			if _, present := payload["changes"]; present {
				t.Fatalf("failure response must not contain changes: %v", payload)
			}
			if _, present := payload["summary"]; present {
				t.Fatalf("failure response must not contain summary: %v", payload)
			}
		})
	}
}

func TestPlanExistingEndpointsUnchanged(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status = %d", rec.Code)
	}
	res := postValidate(t, orderBody(`[`+resEntry("a", `[]`)+`]`))
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("validate got status=%d errors=%v", res.status, res.Errors)
	}
}
