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

// planBody wraps a configuration resources fragment and a priorState
// resources fragment in a full plan request.
func planBody(configResources, stateResources string) string {
	return `{
	  "configuration": {
	    "resourceTypes": [{"name": "t", "schema": {"type": "object", "additionalProperties": true}}],
	    "resources": ` + configResources + `
	  },
	  "priorState": {
	    "resources": ` + stateResources + `
	  }
	}`
}

func stateEntry(address, typ, properties, dependsOn string) string {
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
		t.Fatalf("changes = %v, want empty array", res.Changes)
	}
	if res.Summary == nil || *res.Summary != (planSummary{}) {
		t.Fatalf("summary = %+v, want all zero", res.Summary)
	}
}

func TestPlanCreatesInConfigurationOrder(t *testing.T) {
	// a depends on b and c; both depend on d. Deterministic config order is
	// d, b, c, a; creates must follow it.
	res := postPlan(t, planBody(`[`+
		resEntry("a", `["c", "b"]`)+`,`+
		resEntry("b", `["d"]`)+`,`+
		resEntry("c", `["d"]`)+`,`+
		resEntry("d", `[]`)+`]`, `[]`))
	if res.status != http.StatusOK {
		t.Fatalf("got status=%d errors=%v", res.status, res.Errors)
	}
	if got := changeSummary(*res.Changes); got != "create:d|create:b|create:c|create:a" {
		t.Fatalf("changes = %v", got)
	}
	if *res.Summary != (planSummary{Create: 4}) {
		t.Fatalf("summary = %+v", *res.Summary)
	}
	for _, c := range *res.Changes {
		if c.Before != nil || c.After == nil {
			t.Fatalf("create %s must carry only after: %+v", c.Address, c)
		}
	}
	if c := (*res.Changes)[0]; c.After.Type != "t" || c.After.DependsOn == nil || len(c.After.DependsOn) != 0 {
		t.Fatalf("snapshot = %+v", c.After)
	}
}

func TestPlanCreateSnapshotNormalizesDependsOn(t *testing.T) {
	res := postPlan(t, planBody(`[`+
		resEntry("a", `["c", "b"]`)+`,`+
		resEntry("b", `[]`)+`,`+
		resEntry("c", `[]`)+`]`, `[]`))
	if res.status != http.StatusOK {
		t.Fatalf("got status=%d errors=%v", res.status, res.Errors)
	}
	var a *planChange
	for i := range *res.Changes {
		if (*res.Changes)[i].Address == "a" {
			a = &(*res.Changes)[i]
		}
	}
	if a == nil {
		t.Fatal("no change for a")
	}
	got := strings.Join(a.After.DependsOn, "|")
	if got != "b|c" {
		t.Fatalf("dependsOn = %v, want ascending [b c]", a.After.DependsOn)
	}
}

func TestPlanDeletesInReverseStateOrder(t *testing.T) {
	// State chain a -> b -> c (a depends on b). Topological order is c, b, a;
	// deletes must come reversed: a, b, c.
	res := postPlan(t, planBody(`[]`, `[`+
		stateEntry("a", "t", `{}`, `["b"]`)+`,`+
		stateEntry("b", "t", `{}`, `["c"]`)+`,`+
		stateEntry("c", "t", `{}`, ``)+`]`))
	if res.status != http.StatusOK {
		t.Fatalf("got status=%d errors=%v", res.status, res.Errors)
	}
	if got := changeSummary(*res.Changes); got != "delete:a|delete:b|delete:c" {
		t.Fatalf("changes = %v", got)
	}
	if *res.Summary != (planSummary{Delete: 3}) {
		t.Fatalf("summary = %+v", *res.Summary)
	}
	for _, c := range *res.Changes {
		if c.Before == nil || c.After != nil {
			t.Fatalf("delete %s must carry only before: %+v", c.Address, c)
		}
	}
	if got := strings.Join((*res.Changes)[0].Before.DependsOn, "|"); got != "b" {
		t.Fatalf("before dependsOn = %v", got)
	}
}

func TestPlanDeletesPrecedeCreatesAndUpdates(t *testing.T) {
	res := postPlan(t, planBody(
		`[`+resEntry("keep", `["new"]`)+`,`+resEntry("new", `[]`)+`]`,
		`[`+stateEntry("keep", "t", `{"v": 1}`, ``)+`,`+stateEntry("old", "t", `{}`, `["keep"]`)+`]`))
	if res.status != http.StatusOK {
		t.Fatalf("got status=%d errors=%v", res.status, res.Errors)
	}
	// old is deleted (reverse state topo), then config order new, keep lists
	// the create and the update.
	if got := changeSummary(*res.Changes); got != "delete:old|create:new|update:keep" {
		t.Fatalf("changes = %v", got)
	}
	if *res.Summary != (planSummary{Create: 1, Update: 1, Delete: 1}) {
		t.Fatalf("summary = %+v", *res.Summary)
	}
	update := (*res.Changes)[2]
	if update.Before == nil || update.After == nil {
		t.Fatalf("update must carry before and after: %+v", update)
	}
	if update.Before.DependsOn == nil || len(update.Before.DependsOn) != 0 {
		t.Fatalf("missing dependsOn must snapshot as empty array: %+v", update.Before)
	}
	if got := strings.Join(update.After.DependsOn, "|"); got != "new" {
		t.Fatalf("after dependsOn = %v", got)
	}
}

func TestPlanNoopIgnoresKeyOrderNumberFormAndDependsOnOrder(t *testing.T) {
	res := postPlan(t, planBody(
		`[`+resEntry("a", `["b", "c"]`)+`,`+resEntry("b", `[]`)+`,`+resEntry("c", `[]`)+`]`,
		`[`+
			stateEntry("a", "t", `{"n": 1.0, "s": "x", "list": [1, 2e0], "obj": {"y": 2, "x": 1}}`, `["c", "b"]`)+`,`+
			stateEntry("b", "t", `{}`, `[]`)+`,`+
			stateEntry("c", "t", `{}`, ``)+`]`))
	if res.status != http.StatusOK {
		t.Fatalf("got status=%d errors=%v", res.status, res.Errors)
	}
	// a has no properties in the configuration but the state has some: that
	// is a real diff, so only b and c are noops.
	if got := changeSummary(*res.Changes); got != "update:a" {
		t.Fatalf("changes = %v", got)
	}
	if *res.Summary != (planSummary{Update: 1, Noop: 2}) {
		t.Fatalf("summary = %+v", *res.Summary)
	}
}

func TestPlanNoopWhenFullyEquivalent(t *testing.T) {
	config := `[{"address": "a", "type": "t", "properties": {"n": 1, "list": [1, 2]}, "dependsOn": ["b", "c"]},` +
		resEntry("b", `[]`) + `,` + resEntry("c", `[]`) + `]`
	state := `[` +
		stateEntry("c", "t", `{}`, ``) + `,` +
		stateEntry("a", "t", `{"list": [1.0, 2e0], "n": 1.00}`, `["c", "b"]`) + `,` +
		stateEntry("b", "t", `{}`, `[]`) + `]`
	res := postPlan(t, planBody(config, state))
	if res.status != http.StatusOK {
		t.Fatalf("got status=%d errors=%v", res.status, res.Errors)
	}
	if len(*res.Changes) != 0 {
		t.Fatalf("changes = %v, want none", changeSummary(*res.Changes))
	}
	if *res.Summary != (planSummary{Noop: 3}) {
		t.Fatalf("summary = %+v", *res.Summary)
	}
}

func TestPlanUpdateTriggers(t *testing.T) {
	cases := map[string]struct {
		config string
		state  string
	}{
		"type changed": {
			`[{"address": "a", "type": "t", "properties": {}}]`,
			`[` + stateEntry("a", "other", `{}`, ``) + `]`,
		},
		"property value changed": {
			`[{"address": "a", "type": "t", "properties": {"n": 2}}]`,
			`[` + stateEntry("a", "t", `{"n": 1}`, ``) + `]`,
		},
		"array order matters": {
			`[{"address": "a", "type": "t", "properties": {"list": [1, 2]}}]`,
			`[` + stateEntry("a", "t", `{"list": [2, 1]}`, ``) + `]`,
		},
		"dependsOn set changed": {
			`[` + resEntry("a", `["b"]`) + `,` + resEntry("b", `[]`) + `]`,
			`[` + stateEntry("a", "t", `{}`, ``) + `,` + stateEntry("b", "t", `{}`, ``) + `]`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			res := postPlan(t, planBody(tc.config, tc.state))
			if res.status != http.StatusOK {
				t.Fatalf("got status=%d errors=%v", res.status, res.Errors)
			}
			if got := changeSummary(*res.Changes); !strings.Contains(got, "update:a") {
				t.Fatalf("changes = %v, want update:a", got)
			}
		})
	}
}

func TestPlanStateMayKeepUnknownTypes(t *testing.T) {
	// The state references types the configuration no longer declares; that
	// is not an error.
	res := postPlan(t, planBody(`[]`, `[`+stateEntry("a", "gone", `{"x": 1}`, ``)+`]`))
	if res.status != http.StatusOK {
		t.Fatalf("got status=%d errors=%v", res.status, res.Errors)
	}
	if got := changeSummary(*res.Changes); got != "delete:a" {
		t.Fatalf("changes = %v", got)
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

func TestPlanEnvelopeShapeErrors(t *testing.T) {
	cases := map[string]struct {
		body string
		want []string
	}{
		"not an object": {
			`[]`,
			[]string{"invalid_plan_request@"},
		},
		"missing both fields": {
			`{}`,
			[]string{"invalid_plan_request@", "invalid_plan_request@"},
		},
		"missing priorState": {
			`{"configuration": {"resourceTypes": [], "resources": []}}`,
			[]string{"invalid_plan_request@"},
		},
		"unknown field": {
			`{"configuration": {"resourceTypes": [], "resources": []}, "priorState": {"resources": []}, "extra": 1}`,
			[]string{"invalid_plan_request@/extra"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			res := postPlan(t, tc.body)
			if res.status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422", res.status)
			}
			if res.Valid {
				t.Fatal("valid must be false")
			}
			if got := planErrCodes(res); strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPlanStateShapeErrors(t *testing.T) {
	config := `{"resourceTypes": [], "resources": []}`
	cases := map[string]struct {
		state string
		want  []string
	}{
		"not an object": {
			`[]`,
			[]string{"invalid_plan_request@/priorState"},
		},
		"missing resources": {
			`{}`,
			[]string{"invalid_plan_request@/priorState"},
		},
		"resources not array": {
			`{"resources": {}}`,
			[]string{"invalid_plan_request@/priorState/resources"},
		},
		"entry not object": {
			`{"resources": [1]}`,
			[]string{"invalid_plan_request@/priorState/resources/0"},
		},
		"address problems": {
			`{"resources": [{"type": "t", "properties": {}}, {"address": 3, "type": "t", "properties": {}}, {"address": "", "type": "t", "properties": {}}]}`,
			[]string{
				"invalid_plan_request@/priorState/resources/0",
				"invalid_plan_request@/priorState/resources/1/address",
				"invalid_plan_request@/priorState/resources/2/address",
			},
		},
		"type problems": {
			`{"resources": [{"address": "a", "properties": {}}, {"address": "b", "type": 3, "properties": {}}, {"address": "c", "type": "", "properties": {}}]}`,
			[]string{
				"invalid_plan_request@/priorState/resources/0",
				"invalid_plan_request@/priorState/resources/1/type",
				"invalid_plan_request@/priorState/resources/2/type",
			},
		},
		"properties problems": {
			`{"resources": [{"address": "a", "type": "t"}, {"address": "b", "type": "t", "properties": []}]}`,
			[]string{
				"invalid_plan_request@/priorState/resources/0",
				"invalid_plan_request@/priorState/resources/1/properties",
			},
		},
		"duplicate address": {
			`{"resources": [` + stateEntry("a", "t", `{}`, ``) + `,` + stateEntry("a", "t", `{}`, ``) + `]}`,
			[]string{"duplicate_identifier@/priorState/resources/1/address"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			body := `{"configuration": ` + config + `, "priorState": ` + tc.state + `}`
			res := postPlan(t, body)
			if res.status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (errors %v)", res.status, res.Errors)
			}
			if got := planErrCodes(res); strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPlanStateDependencyErrors(t *testing.T) {
	config := `{"resourceTypes": [], "resources": []}`
	cases := map[string]struct {
		resources string
		want      []string
	}{
		"shape": {
			`[` + stateEntry("a", "t", `{}`, `{}`) + `,` + stateEntry("b", "t", `{}`, `["a", 3, "", "a"]`) + `]`,
			[]string{
				"invalid_document@/priorState/resources/0/dependsOn",
				"invalid_document@/priorState/resources/1/dependsOn/1",
				"invalid_document@/priorState/resources/1/dependsOn/2",
				"duplicate_dependency@/priorState/resources/1/dependsOn/3",
			},
		},
		"self and unknown": {
			`[` + stateEntry("a", "t", `{}`, `["a"]`) + `,` + stateEntry("b", "t", `{}`, `["ghost", "a"]`) + `]`,
			[]string{
				"self_dependency@/priorState/resources/0/dependsOn/0",
				"unknown_dependency@/priorState/resources/1/dependsOn/0",
			},
		},
		"cycle": {
			`[` + stateEntry("a", "t", `{}`, `["b"]`) + `,` + stateEntry("b", "t", `{}`, `["a"]`) + `]`,
			[]string{"dependency_cycle@/priorState/resources"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			body := `{"configuration": ` + config + `, "priorState": {"resources": ` + tc.resources + `}}`
			res := postPlan(t, body)
			if res.status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (errors %v)", res.status, res.Errors)
			}
			if got := planErrCodes(res); strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPlanStateShapeFailureSuppressesDependencyErrors(t *testing.T) {
	// The duplicate address is a shape failure; the malformed dependsOn must
	// not add dependency findings.
	body := `{"configuration": {"resourceTypes": [], "resources": []}, "priorState": {"resources": [` +
		stateEntry("a", "t", `{}`, `{}`) + `,` + stateEntry("a", "t", `{}`, `["nope"]`) + `]}}`
	res := postPlan(t, body)
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", res.status)
	}
	want := []string{"duplicate_identifier@/priorState/resources/1/address"}
	if got := planErrCodes(res); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestPlanConfigurationErrorsGetPrefixed(t *testing.T) {
	cases := map[string]struct {
		config string
		want   []string
	}{
		"validation": {
			`{"resourceTypes": [], "resources": [{"address": "a", "type": "ghost", "properties": {}}]}`,
			[]string{"unknown_resource_type@/configuration/resources/0/type"},
		},
		"not an object": {
			`[]`,
			[]string{"invalid_document@/configuration"},
		},
		"dependency": {
			`{"resourceTypes": [{"name": "t", "schema": {"type": "object"}}], "resources": [` +
				resEntry("a", `["ghost"]`) + `]}`,
			[]string{"unknown_dependency@/configuration/resources/0/dependsOn/0"},
		},
		"cycle": {
			`{"resourceTypes": [{"name": "t", "schema": {"type": "object"}}], "resources": [` +
				resEntry("a", `["b"]`) + `,` + resEntry("b", `["a"]`) + `]}`,
			[]string{"dependency_cycle@/configuration/resources"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			body := `{"configuration": ` + tc.config + `, "priorState": {"resources": []}}`
			res := postPlan(t, body)
			if res.status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (errors %v)", res.status, res.Errors)
			}
			if got := planErrCodes(res); strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPlanErrorsSortedAcrossSources(t *testing.T) {
	// Configuration and state findings merge into one list sorted by path,
	// then code: /configuration/... sorts before /priorState/...
	body := `{
	  "configuration": {"resourceTypes": [], "resources": [{"address": "a", "type": "ghost", "properties": {}}]},
	  "priorState": {"resources": [{"address": "a", "type": "t"}, {"address": "b", "type": "t", "properties": {}}]}
	}`
	res := postPlan(t, body)
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", res.status)
	}
	want := []string{
		"unknown_resource_type@/configuration/resources/0/type",
		"invalid_plan_request@/priorState/resources/0",
	}
	if got := planErrCodes(res); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestPlanFailureResponsesOmitChangesAndSummary(t *testing.T) {
	cases := map[string]string{
		"invalid_json": `{`,
		"envelope":     `{}`,
		"config":       `{"configuration": {"resourceTypes": [], "resources": [{"address":"a","type":"x","properties":{}}]}, "priorState": {"resources": []}}`,
		"state":        `{"configuration": {"resourceTypes": [], "resources": []}, "priorState": {"resources": [{}]}}`,
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
	// The plan request document is not accepted by the configuration
	// endpoints, and they keep working on plain configuration documents.
	res := postValidate(t, orderBody(`[`+resEntry("a", `["b"]`)+`,`+resEntry("b", `[]`)+`]`))
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("validate got status=%d errors=%v", res.status, res.Errors)
	}
	order := postOrder(t, orderBody(`[`+resEntry("a", `["b"]`)+`,`+resEntry("b", `[]`)+`]`))
	if order.status != http.StatusOK || !order.Valid {
		t.Fatalf("order got status=%d errors=%v", order.status, order.Errors)
	}
}
