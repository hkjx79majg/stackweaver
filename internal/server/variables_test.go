package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
)

// variableConfig builds a configuration document with the given variable
// sections and resources fragment.
func variableConfig(variables, variableValues, resources string) string {
	doc := `{`
	doc += `"resourceTypes": [{"name": "t", "schema": {"type": "object", "additionalProperties": true}}],`
	if variables != "" {
		doc += `"variables": ` + variables + `,`
	}
	if variableValues != "" {
		doc += `"variableValues": ` + variableValues + `,`
	}
	doc += `"resources": ` + resources + `}`
	return doc
}

func TestValidateVariablesAcceptsResolvedReferences(t *testing.T) {
	body := variableConfig(
		`{"size": {"schema": {"type": "integer"}, "default": 2},
		  "label": {"schema": {"type": "string"}},
		  "tags": {"schema": {"type": "object", "additionalProperties": true}, "default": {"team": "core"}}}`,
		`{"label": "web"}`,
		`[{"address": "a", "type": "t", "properties": {
			"cpu": {"$variable": "size"},
			"name": {"$variable": "label"},
			"nested": {"list": [{"$variable": "size"}, {"$variable": "tags"}]}
		}}]`,
	)
	res := postValidate(t, body)
	if res.status != http.StatusOK || !res.Valid || len(res.Errors) != 0 {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
}

func TestValidateVariablesStructure(t *testing.T) {
	cases := map[string]struct {
		body string
		want []string
	}{
		"variables not object": {
			variableConfig(`"nope"`, "", `[]`),
			[]string{"invalid_document@/variables"},
		},
		"variableValues not object": {
			variableConfig("", `[1]`, `[]`),
			[]string{"invalid_document@/variableValues"},
		},
		"declaration not object": {
			variableConfig(`{"size": 3}`, "", `[]`),
			[]string{"invalid_document@/variables/size"},
		},
		"declaration unknown field": {
			variableConfig(`{"size": {"schema": {"type": "integer"}, "extra": 1}}`, "", `[]`),
			[]string{"invalid_document@/variables/size/extra"},
		},
		"declaration missing schema": {
			variableConfig(`{"size": {"default": 1}}`, "", `[]`),
			[]string{"invalid_document@/variables/size"},
		},
		"declaration invalid schema": {
			variableConfig(`{"size": {"schema": {"type": "gadget"}}}`, "", `[]`),
			[]string{"invalid_document@/variables/size/schema/type"},
		},
		"empty variable name": {
			variableConfig(`{"": {"schema": {"type": "integer"}}}`, "", `[]`),
			[]string{"invalid_document@/variables/"},
		},
		"default violates schema": {
			variableConfig(`{"size": {"schema": {"type": "integer"}, "default": "big"}}`, "", `[]`),
			[]string{"type_mismatch@/variables/size/default"},
		},
		"explicit value violates schema": {
			variableConfig(`{"size": {"schema": {"type": "integer"}}}`, `{"size": "big"}`, `[]`),
			[]string{"type_mismatch@/variableValues/size"},
		},
		"explicit value for undeclared variable": {
			variableConfig("", `{"ghost": 1}`, `[]`),
			[]string{"unknown_variable@/variableValues/ghost"},
		},
		"unreferenced declaration still validated": {
			variableConfig(`{"unused": {"schema": {"type": "string"}, "default": 4}}`, "", `[]`),
			[]string{"type_mismatch@/variables/unused/default"},
		},
		"unreferenced explicit value still validated": {
			variableConfig(`{"unused": {"schema": {"type": "boolean"}}}`, `{"unused": 4}`, `[]`),
			[]string{"type_mismatch@/variableValues/unused"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			res := postValidate(t, tc.body)
			if res.status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (errors %v)", res.status, res.Errors)
			}
			if got := errCodes(res); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("errors = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidateVariableReferences(t *testing.T) {
	decls := `{"size": {"schema": {"type": "integer"}, "default": 2},
	           "novalue": {"schema": {"type": "integer"}}}`
	cases := map[string]struct {
		properties string
		want       []string
	}{
		"reference with extra fields": {
			`{"cpu": {"$variable": "size", "other": 1}}`,
			[]string{"invalid_variable_reference@/resources/0/properties/cpu"},
		},
		"reference value not string": {
			`{"cpu": {"$variable": 3}}`,
			[]string{"invalid_variable_reference@/resources/0/properties/cpu"},
		},
		"reference value empty": {
			`{"cpu": {"$variable": ""}}`,
			[]string{"invalid_variable_reference@/resources/0/properties/cpu"},
		},
		"unknown variable": {
			`{"cpu": {"$variable": "ghost"}}`,
			[]string{"unknown_variable@/resources/0/properties/cpu"},
		},
		"declared without value": {
			`{"cpu": {"$variable": "novalue"}}`,
			[]string{"missing_variable_value@/variables/novalue"},
		},
		"nested unknown variable": {
			`{"list": [{"ok": 1}, {"cpu": {"$variable": "ghost"}}]}`,
			[]string{"unknown_variable@/resources/0/properties/list/1/cpu"},
		},
		"reference errors suppress property validation": {
			// cpu is declared as an integer below; without the reference
			// error this would also be a type_mismatch.
			`{"cpu": {"$variable": "ghost"}, "name": 5}`,
			[]string{"unknown_variable@/resources/0/properties/cpu"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			body := `{
			  "resourceTypes": [{"name": "t", "schema": {"type": "object",
			    "properties": {"cpu": {"type": "integer"}, "name": {"type": "string"}, "list": {"type": "array", "items": {"type": "object", "additionalProperties": true}}},
			    "additionalProperties": false}}],
			  "variables": ` + decls + `,
			  "resources": [{"address": "a", "type": "t", "properties": ` + tc.properties + `}]
			}`
			res := postValidate(t, body)
			if res.status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (errors %v)", res.status, res.Errors)
			}
			if got := errCodes(res); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("errors = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidateVariablesResolvedPropertiesAreChecked(t *testing.T) {
	// The default for size is a string, so after resolution cpu holds a
	// string and fails the integer schema of the resource type.
	body := `{
	  "resourceTypes": [{"name": "t", "schema": {"type": "object",
	    "properties": {"cpu": {"type": "integer"}}, "additionalProperties": false}}],
	  "variables": {"size": {"schema": {"type": "string"}, "default": "big"}},
	  "resources": [{"address": "a", "type": "t", "properties": {"cpu": {"$variable": "size"}}}]
	}`
	res := postValidate(t, body)
	want := []string{"type_mismatch@/resources/0/properties/cpu"}
	if res.status != http.StatusUnprocessableEntity || !reflect.DeepEqual(errCodes(res), want) {
		t.Fatalf("got status=%d errors=%v, want 422 %v", res.status, errCodes(res), want)
	}
}

func TestValidateVariablesLiteralValuesAreNotResolved(t *testing.T) {
	// A default that itself looks like a reference is stored literally and
	// never recursively resolved.
	body := variableConfig(
		`{"raw": {"schema": {"type": "object", "additionalProperties": true}, "default": {"$variable": "size"}},
		  "size": {"schema": {"type": "integer"}, "default": 1}}`,
		"",
		`[{"address": "a", "type": "t", "properties": {"v": {"$variable": "raw"}}}]`,
	)
	res := postValidate(t, body)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
}

func TestValidateWithoutVariableFieldsKeepsLegacyBehavior(t *testing.T) {
	// Without the variable fields a "$variable"-shaped property is ordinary
	// data: it passes an open schema and is reported as an unexpected
	// property under a closed one, exactly as before variables existed.
	open := `{
	  "resourceTypes": [{"name": "t", "schema": {"type": "object", "additionalProperties": true}}],
	  "resources": [{"address": "a", "type": "t", "properties": {"cpu": {"$variable": "size"}}}]
	}`
	if res := postValidate(t, open); res.status != http.StatusOK || !res.Valid {
		t.Fatalf("open schema: got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
	closed := `{
	  "resourceTypes": [{"name": "t", "schema": {"type": "object", "properties": {"cpu": {"type": "object", "additionalProperties": true}}, "additionalProperties": false}}],
	  "resources": [{"address": "a", "type": "t", "properties": {"cpu": {"$variable": "size"}, "extra": 1}}]
	}`
	res := postValidate(t, closed)
	want := []string{"unexpected_property@/resources/0/properties/extra"}
	if res.status != http.StatusUnprocessableEntity || !reflect.DeepEqual(errCodes(res), want) {
		t.Fatalf("closed schema: got status=%d errors=%v, want 422 %v", res.status, errCodes(res), want)
	}
}

// variablePlanBody wraps a configuration with variables and a priorState in
// a plan request envelope.
func variablePlanBody(variables, variableValues, configResources, stateResources string) string {
	config := `{`
	config += `"resourceTypes": [{"name": "t", "schema": {"type": "object", "additionalProperties": true}}],`
	if variables != "" {
		config += `"variables": ` + variables + `,`
	}
	if variableValues != "" {
		config += `"variableValues": ` + variableValues + `,`
	}
	config += `"resources": ` + configResources + `}`
	return `{"configuration": ` + config + `, "priorState": {"resources": ` + stateResources + `}}`
}

func TestPlanResolvesVariables(t *testing.T) {
	body := variablePlanBody(
		`{"size": {"schema": {"type": "integer"}, "default": 2},
		  "label": {"schema": {"type": "string"}, "default": "unset"}}`,
		`{"label": "web"}`,
		`[{"address": "a", "type": "t", "properties": {"cpu": {"$variable": "size"}, "name": {"$variable": "label"}}}]`,
		`[]`,
	)
	res := postPlan(t, body)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
	if res.Changes == nil || len(*res.Changes) != 1 {
		t.Fatalf("changes = %v, want exactly one", res.Changes)
	}
	change := (*res.Changes)[0]
	if change.Action != "create" || change.After == nil {
		t.Fatalf("change = %+v, want a create with an after snapshot", change)
	}
	wantProps := map[string]any{"cpu": float64(2), "name": "web"}
	if !reflect.DeepEqual(change.After.Properties, wantProps) {
		t.Fatalf("after properties = %v, want %v", change.After.Properties, wantProps)
	}
}

func TestPlanVariableExplicitValueWinsOverDefault(t *testing.T) {
	body := variablePlanBody(
		`{"size": {"schema": {"type": "integer"}, "default": 2}}`,
		`{"size": 8}`,
		`[{"address": "a", "type": "t", "properties": {"cpu": {"$variable": "size"}}}]`,
		`[]`,
	)
	res := postPlan(t, body)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
	got := (*res.Changes)[0].After.Properties["cpu"]
	if got != float64(8) {
		t.Fatalf("cpu = %v, want 8 (explicit value must win over default)", got)
	}
}

func TestPlanComparesAgainstResolvedValues(t *testing.T) {
	// The prior state holds the resolved values, so the same configuration
	// expressed through variables is a noop.
	body := variablePlanBody(
		`{"size": {"schema": {"type": "integer"}, "default": 2}}`,
		"",
		`[{"address": "a", "type": "t", "properties": {"cpu": {"$variable": "size"}}}]`,
		`[{"address": "a", "type": "t", "properties": {"cpu": 2}}]`,
	)
	res := postPlan(t, body)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
	if res.Changes == nil || len(*res.Changes) != 0 {
		t.Fatalf("changes = %v, want none (noop)", res.Changes)
	}
	if res.Summary == nil || res.Summary.Noop != 1 {
		t.Fatalf("summary = %+v, want one noop", res.Summary)
	}
}

func TestPlanVariableErrorsArePrefixed(t *testing.T) {
	body := variablePlanBody(
		`{"size": {"schema": {"type": "integer"}}}`,
		"",
		`[{"address": "a", "type": "t", "properties": {"cpu": {"$variable": "size"}}}]`,
		`[]`,
	)
	res := postPlan(t, body)
	want := []string{"missing_variable_value@/configuration/variables/size"}
	if res.status != http.StatusUnprocessableEntity || !reflect.DeepEqual(planErrCodes(res), want) {
		t.Fatalf("got status=%d errors=%v, want 422 %v", res.status, planErrCodes(res), want)
	}
}

func TestApplyResolvesVariables(t *testing.T) {
	provider := &recordingProvider{}
	handler := HandlerWithProvider(provider)
	body := variablePlanBody(
		`{"size": {"schema": {"type": "integer"}, "default": 2}}`,
		"",
		`[{"address": "a", "type": "t", "properties": {"cpu": {"$variable": "size"}}}]`,
		`[]`,
	)
	res := doApply(t, handler, body, nil)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
	if len(provider.calls) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(provider.calls))
	}
	wantProps := map[string]any{"cpu": json.Number("2")}
	if !reflect.DeepEqual(provider.calls[0].After.Properties, wantProps) {
		t.Fatalf("provider after properties = %v, want %v", provider.calls[0].After.Properties, wantProps)
	}
	if res.State == nil || len(res.State.Resources) != 1 {
		t.Fatalf("state = %+v, want one resource", res.State)
	}
	if got := res.State.Resources[0].Properties["cpu"]; got != float64(2) {
		t.Fatalf("state cpu = %v, want 2", got)
	}
	// The variable sections must not be echoed anywhere in the response.
	if raw := res.raw; strings.Contains(mustMarshal(t, raw), "$variable") {
		t.Fatalf("response echoes variable references: %v", raw)
	}
}

func TestApplyVariableErrorsNeverCallProvider(t *testing.T) {
	provider := &recordingProvider{}
	handler := HandlerWithProvider(provider)
	body := variablePlanBody(
		"",
		`{"ghost": 1}`,
		`[{"address": "a", "type": "t", "properties": {"cpu": 1}}]`,
		`[]`,
	)
	res := doApply(t, handler, body, nil)
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", res.status)
	}
	if len(provider.calls) != 0 {
		t.Fatalf("provider was called %d times for an invalid document", len(provider.calls))
	}
}

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(data)
}

func TestStateApplyStoresResolvedProperties(t *testing.T) {
	provider := &recordingProvider{}
	path := stateFilePath(t)
	handler := HandlerWithProviderAndStateFile(provider, path)

	body := `{
	  "configuration": {
	    "resourceTypes": [{"name": "t", "schema": {"type": "object", "additionalProperties": true}}],
	    "variables": {"size": {"schema": {"type": "integer"}, "default": 2}},
	    "resources": [{"address": "a", "type": "t", "properties": {"cpu": {"$variable": "size"}}}]
	  }
	}`
	res := doStateApply(t, handler, body)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}

	// The state file holds the resolved properties and no variable sections.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	if strings.Contains(string(data), "$variable") || strings.Contains(string(data), "variables") {
		t.Fatalf("state file echoes variable syntax: %s", data)
	}
	get := doStateGet(t, handler)
	if get.status != http.StatusOK || get.State == nil || len(get.State.Resources) != 1 {
		t.Fatalf("GET state = %+v, want one resource", get)
	}
	if got := get.State.Resources[0].Properties["cpu"]; got != float64(2) {
		t.Fatalf("stored cpu = %v, want 2", got)
	}

	// A follow-up apply of the literal resolved configuration is an empty
	// plan and leaves the revision untouched.
	literal := `{
	  "configuration": {
	    "resourceTypes": [{"name": "t", "schema": {"type": "object", "additionalProperties": true}}],
	    "resources": [{"address": "a", "type": "t", "properties": {"cpu": 2}}]
	  }
	}`
	second := doStateApply(t, handler, literal)
	if second.status != http.StatusOK || !second.Valid {
		t.Fatalf("second apply: status=%d valid=%v errors=%v", second.status, second.Valid, second.Errors)
	}
	if second.Summary == nil || second.Summary.Noop != 1 {
		t.Fatalf("second apply summary = %+v, want one noop", second.Summary)
	}
	if second.Revision == nil || *second.Revision != 1 {
		t.Fatalf("second apply revision = %v, want 1", second.Revision)
	}
	if len(provider.calls) != 1 {
		t.Fatalf("provider calls = %d, want 1 (the noop run must not call it)", len(provider.calls))
	}
}

func TestStateApplyVariableErrorsLeaveFileUntouched(t *testing.T) {
	provider := &recordingProvider{}
	path := stateFilePath(t)
	handler := HandlerWithProviderAndStateFile(provider, path)

	body := `{
	  "configuration": {
	    "resourceTypes": [{"name": "t", "schema": {"type": "object", "additionalProperties": true}}],
	    "variables": {"size": {"schema": {"type": "integer"}}},
	    "resources": [{"address": "a", "type": "t", "properties": {"cpu": {"$variable": "size"}}}]
	  }
	}`
	res := doStateApply(t, handler, body)
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", res.status)
	}
	if len(provider.calls) != 0 {
		t.Fatalf("provider was called for an invalid document")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("state file exists after a rejected run: %v", err)
	}
	get := doStateGet(t, handler)
	if get.status != http.StatusOK || get.Revision != 0 {
		t.Fatalf("GET after rejection: status=%d revision=%d, want revision 0", get.status, get.Revision)
	}
}

func TestOrderAcceptsVariables(t *testing.T) {
	body := variableConfig(
		`{"size": {"schema": {"type": "integer"}, "default": 2}}`,
		"",
		`[{"address": "b", "type": "t", "properties": {"cpu": {"$variable": "size"}}, "dependsOn": ["a"]},
		  {"address": "a", "type": "t", "properties": {}}]`,
	)
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/configurations/order", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var res orderResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(res.Order, []string{"a", "b"}) {
		t.Fatalf("order = %v, want [a b]", res.Order)
	}
}

func TestOrderRejectsVariableErrors(t *testing.T) {
	body := variableConfig(
		"",
		`{"ghost": 1}`,
		`[{"address": "a", "type": "t", "properties": {}}]`,
	)
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/configurations/order", strings.NewReader(body)))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", rec.Code, rec.Body.String())
	}
	var res validateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != "unknown_variable" || res.Errors[0].Path != "/variableValues/ghost" {
		t.Fatalf("errors = %v, want unknown_variable@/variableValues/ghost", res.Errors)
	}
}
