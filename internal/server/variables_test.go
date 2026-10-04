package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// variableConfig is a small valid configuration exercising two variables:
// size with a default, and label with only an explicit value.
const variableConfig = `{
  "resourceTypes": [
    {"name": "vm", "schema": {"type": "object",
      "properties": {
        "cpu": {"type": "integer"},
        "label": {"type": "string"},
        "tags": {"type": "array", "items": {"type": "string"}}
      },
      "required": ["cpu", "label"]}}
  ],
  "variables": {
    "size": {"schema": {"type": "integer"}, "default": 2},
    "label": {"schema": {"type": "string"}}
  },
  "variableValues": {"label": "web"},
  "resources": [
    {"address": "vm.a", "type": "vm", "properties": {
      "cpu": {"$variable": "size"},
      "label": {"$variable": "label"},
      "tags": ["x", {"$variable": "label"}]
    }}
  ]
}`

func TestValidateResolvesVariables(t *testing.T) {
	res := postValidate(t, variableConfig)
	if res.status != http.StatusOK || !res.Valid || len(res.Errors) != 0 {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
}

func TestValidateVariableDeclarationProblems(t *testing.T) {
	for name, tc := range map[string]struct {
		body  string
		codes []string
	}{
		"variablesNotObject": {
			`{"resourceTypes": [], "resources": [], "variables": 4}`,
			[]string{"invalid_document@/variables"},
		},
		"variableValuesNotObject": {
			`{"resourceTypes": [], "resources": [], "variableValues": [1]}`,
			[]string{"invalid_document@/variableValues"},
		},
		"emptyName": {
			`{"resourceTypes": [], "resources": [], "variables": {"": {"schema": {"type": "string"}}}}`,
			[]string{"invalid_document@/variables/"},
		},
		"declarationNotObject": {
			`{"resourceTypes": [], "resources": [], "variables": {"x": "s"}}`,
			[]string{"invalid_document@/variables/x"},
		},
		"missingSchema": {
			`{"resourceTypes": [], "resources": [], "variables": {"x": {"default": 1}}}`,
			[]string{"invalid_document@/variables/x"},
		},
		"extraField": {
			`{"resourceTypes": [], "resources": [], "variables": {"x": {"schema": {"type": "string"}, "bogus": 1}}}`,
			[]string{"invalid_document@/variables/x/bogus"},
		},
		"badSchema": {
			`{"resourceTypes": [], "resources": [], "variables": {"x": {"schema": {"type": "nope"}}}}`,
			[]string{"invalid_document@/variables/x/schema/type"},
		},
		"unknownValueKey": {
			`{"resourceTypes": [], "resources": [], "variables": {"x": {"schema": {"type": "string"}}}, "variableValues": {"y": "a"}}`,
			[]string{"unknown_variable@/variableValues/y"},
		},
		"badDefault": {
			`{"resourceTypes": [], "resources": [], "variables": {"x": {"schema": {"type": "integer"}, "default": "s"}}}`,
			[]string{"type_mismatch@/variables/x/default"},
		},
		"badExplicitValue": {
			`{"resourceTypes": [], "resources": [], "variables": {"x": {"schema": {"type": "integer"}}}, "variableValues": {"x": 1.5}}`,
			[]string{"type_mismatch@/variableValues/x"},
		},
		"unreferencedBadValue": {
			`{"resourceTypes": [], "resources": [], "variables": {"x": {"schema": {"type": "string"}}}, "variableValues": {"x": 3}}`,
			[]string{"type_mismatch@/variableValues/x"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			res := postValidate(t, tc.body)
			if res.status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (errors %v)", res.status, res.Errors)
			}
			got := errCodes(res)
			if len(got) != len(tc.codes) {
				t.Fatalf("got %v, want %v", got, tc.codes)
			}
			for i := range got {
				if got[i] != tc.codes[i] {
					t.Fatalf("got %v, want %v", got, tc.codes)
				}
			}
		})
	}
}

func TestValidateVariableReferenceProblems(t *testing.T) {
	base := func(props string) string {
		return `{"resourceTypes": [{"name": "t", "schema": {"type": "object", "additionalProperties": true}}],` +
			`"resources": [{"address": "a", "type": "t", "properties": ` + props + `}],`
	}
	for name, tc := range map[string]struct {
		body  string
		codes []string
	}{
		"extraField": {
			base(`{"p": {"$variable": "x", "other": 1}}`) + `"variables": {"x": {"schema": {"type": "string"}, "default": "v"}}}`,
			[]string{"invalid_variable_reference@/resources/0/properties/p"},
		},
		"nonStringName": {
			base(`{"p": {"$variable": 3}}`) + `"variables": {"x": {"schema": {"type": "string"}, "default": "v"}}}`,
			[]string{"invalid_variable_reference@/resources/0/properties/p"},
		},
		"emptyName": {
			base(`{"p": {"$variable": ""}}`) + `"variables": {"x": {"schema": {"type": "string"}, "default": "v"}}}`,
			[]string{"invalid_variable_reference@/resources/0/properties/p"},
		},
		"unknownName": {
			base(`{"p": {"$variable": "nope"}}`) + `"variables": {"x": {"schema": {"type": "string"}, "default": "v"}}}`,
			[]string{"unknown_variable@/resources/0/properties/p"},
		},
		"missingValue": {
			base(`{"p": {"$variable": "x"}}`) + `"variables": {"x": {"schema": {"type": "string"}}}}`,
			[]string{"missing_variable_value@/variables/x"},
		},
		"nestedUnknown": {
			base(`{"p": {"q": [{"$variable": "nope"}]}}`) + `"variables": {}}`,
			[]string{"unknown_variable@/resources/0/properties/p/q/0"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			res := postValidate(t, tc.body)
			if res.status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (errors %v)", res.status, res.Errors)
			}
			got := errCodes(res)
			if len(got) != len(tc.codes) {
				t.Fatalf("got %v, want %v", got, tc.codes)
			}
			for i := range got {
				if got[i] != tc.codes[i] {
					t.Fatalf("got %v, want %v", got, tc.codes)
				}
			}
		})
	}
}

// A "$variable" key in a document without the variables/variableValues
// fields keeps the historical behavior: it is ordinary property data.
func TestValidateDollarVariableLegacyDocument(t *testing.T) {
	res := postValidate(t, `{"resourceTypes": [{"name": "t", "schema": {"type": "object", "additionalProperties": true}}],
		"resources": [{"address": "a", "type": "t", "properties": {"p": {"$variable": "x"}}}]}`)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
}

// Explicit values win over defaults, and the resolved properties are what
// property validation sees.
func TestValidateResolvedValueCheckedAgainstResourceSchema(t *testing.T) {
	res := postValidate(t, `{
	  "resourceTypes": [{"name": "t", "schema": {"type": "object", "properties": {"n": {"type": "integer"}}}}],
	  "variables": {"x": {"schema": {"type": "integer"}, "default": 1}},
	  "variableValues": {"x": "not-an-integer"},
	  "resources": [{"address": "a", "type": "t", "properties": {"n": {"$variable": "x"}}}]
	}`)
	// The explicit value fails the variable schema; property validation is
	// suppressed because the variable phase already failed.
	if res.status != http.StatusUnprocessableEntity || len(res.Errors) != 1 || res.Errors[0].Code != "type_mismatch" {
		t.Fatalf("got status=%d errors=%v", res.status, res.Errors)
	}
}

// Reference-shaped objects inside defaults and explicit values are literal
// data, never resolved recursively.
func TestValidateValuesAreLiteral(t *testing.T) {
	res := postValidate(t, `{
	  "resourceTypes": [{"name": "t", "schema": {"type": "object", "properties": {"p": {"type": "object", "additionalProperties": true}}}}],
	  "variables": {
	    "x": {"schema": {"type": "object", "additionalProperties": true}, "default": {"$variable": "y"}}
	  },
	  "resources": [{"address": "a", "type": "t", "properties": {"p": {"$variable": "x"}}}]
	}`)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
}

// The plan endpoint diffs the resolved configuration: after snapshots carry
// literal properties and no variable sections.
func TestPlanUsesResolvedProperties(t *testing.T) {
	config := `{
	  "resourceTypes": [{"name": "vm", "schema": {"type": "object", "properties": {"cpu": {"type": "integer"}, "label": {"type": "string"}}, "required": ["cpu", "label"]}}],
	  "variables": {"size": {"schema": {"type": "integer"}, "default": 2}},
	  "variableValues": {"size": 4},
	  "resources": [{"address": "vm.a", "type": "vm", "properties": {"cpu": {"$variable": "size"}, "label": "x"}}]
	}`
	res := postPlan(t, `{"configuration": `+config+`, "priorState": {"resources": []}}`)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d errors=%v", res.status, res.Errors)
	}
	if res.Changes == nil || len(*res.Changes) != 1 || (*res.Changes)[0].Action != "create" {
		t.Fatalf("unexpected changes: %+v", res.Changes)
	}
	after := (*res.Changes)[0].After
	if after == nil {
		t.Fatal("create change must carry after")
	}
	cpu, ok := after.Properties["cpu"].(float64)
	if !ok || cpu != 4 {
		t.Fatalf("after cpu = %v, want resolved explicit value 4", after.Properties["cpu"])
	}
	if _, leaked := after.Properties["$variable"]; leaked {
		t.Fatalf("after properties must not retain references: %v", after.Properties)
	}
}

// A plan against a prior state holding the resolved values is a noop.
func TestPlanComparesResolvedValues(t *testing.T) {
	config := `{
	  "resourceTypes": [{"name": "vm", "schema": {"type": "object", "properties": {"cpu": {"type": "integer"}}}}],
	  "variables": {"size": {"schema": {"type": "integer"}, "default": 2}},
	  "resources": [{"address": "vm.a", "type": "vm", "properties": {"cpu": {"$variable": "size"}}}]
	}`
	res := postPlan(t, `{"configuration": `+config+`, "priorState": {"resources": [{"address": "vm.a", "type": "vm", "properties": {"cpu": 2}}]}}`)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d errors=%v", res.status, res.Errors)
	}
	if res.Summary == nil || res.Summary.Noop != 1 || res.Changes == nil || len(*res.Changes) != 0 {
		t.Fatalf("got summary=%+v changes=%v, want one noop", res.Summary, res.Changes)
	}
}

// Variable errors surface through the plan envelope with the /configuration
// prefix and never reach a Provider.
func TestPlanReportsVariableErrorsWithPrefix(t *testing.T) {
	config := `{
	  "resourceTypes": [{"name": "vm", "schema": {"type": "object", "additionalProperties": true}}],
	  "resources": [{"address": "vm.a", "type": "vm", "properties": {"cpu": {"$variable": "nope"}}}],
	  "variables": {}
	}`
	res := postPlan(t, `{"configuration": `+config+`, "priorState": {"resources": []}}`)
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != "unknown_variable" || res.Errors[0].Path != "/configuration/resources/0/properties/cpu" {
		t.Fatalf("unexpected errors: %+v", res.Errors)
	}
}

// The order endpoint applies the same variable semantics as validate.
func TestOrderAppliesVariableSemantics(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/configurations/order", strings.NewReader(`{
	  "resourceTypes": [{"name": "t", "schema": {"type": "object", "additionalProperties": true}}],
	  "resources": [{"address": "a", "type": "t", "properties": {"p": {"$variable": "x"}}}],
	  "variables": {}
	}`)))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", rec.Code, rec.Body.String())
	}
	var res validateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != "unknown_variable" {
		t.Fatalf("unexpected errors: %+v", res.Errors)
	}
}

// The apply endpoint hands Providers the resolved snapshots.
func TestApplyProviderReceivesResolvedSnapshot(t *testing.T) {
	provider := &recordingProvider{}
	handler := HandlerWithProvider(provider)
	body := `{"configuration": {
	  "resourceTypes": [{"name": "t", "schema": {"type": "object", "properties": {"n": {"type": "integer"}}}}],
	  "variables": {"x": {"schema": {"type": "integer"}, "default": 7}},
	  "resources": [{"address": "a", "type": "t", "properties": {"n": {"$variable": "x"}}}]
	}, "priorState": {"resources": []}}`
	res := doApply(t, handler, body, nil)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d errors=%v", res.status, res.Errors)
	}
	if len(provider.calls) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(provider.calls))
	}
	got := provider.calls[0].After.Properties["n"]
	if num, ok := got.(json.Number); !ok || num.String() != "7" {
		t.Fatalf("provider saw n = %#v, want resolved 7", got)
	}
}

// Variable errors abort apply before any Provider call.
func TestApplyVariableErrorDoesNotCallProvider(t *testing.T) {
	provider := &recordingProvider{}
	handler := HandlerWithProvider(provider)
	body := `{"configuration": {
	  "resourceTypes": [{"name": "t", "schema": {"type": "object", "additionalProperties": true}}],
	  "variables": {},
	  "resources": [{"address": "a", "type": "t", "properties": {"n": {"$variable": "x"}}}]
	}, "priorState": {"resources": []}}`
	res := doApply(t, handler, body, nil)
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (errors %v)", res.status, res.Errors)
	}
	if len(provider.calls) != 0 {
		t.Fatalf("provider must not be called, got %d calls", len(provider.calls))
	}
}

// The state-file apply stores resolved properties, so a later plan against
// the stored state compares literal values.
func TestStateApplyStoresResolvedProperties(t *testing.T) {
	path := stateFilePath(t)
	provider := &recordingProvider{}
	handler := HandlerWithProviderAndStateFile(provider, path)
	body := `{
	  "configuration": {
	    "resourceTypes": [{"name": "t", "schema": {"type": "object", "properties": {"n": {"type": "integer"}}}}],
	    "variables": {"x": {"schema": {"type": "integer"}, "default": 5}},
	    "resources": [{"address": "a", "type": "t", "properties": {"n": {"$variable": "x"}}}]
	  }
	}`
	res := doStateApply(t, handler, body)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d errors=%v", res.status, res.Errors)
	}
	if res.State == nil || len(res.State.Resources) != 1 {
		t.Fatalf("unexpected state: %+v", res.State)
	}
	if got := res.State.Resources[0].Properties["n"]; got != float64(5) {
		t.Fatalf("response state n = %#v, want resolved 5", got)
	}

	_, resources := readStateFileRaw(t, path)
	if len(resources) != 1 {
		t.Fatalf("state file resources = %d, want 1", len(resources))
	}
	if got := resources[0].Properties["n"]; got != float64(5) {
		t.Fatalf("state file n = %#v, want resolved 5", got)
	}

	// Reapplying the same configuration is a noop against the stored
	// resolved values.
	res = doStateApply(t, handler, body)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("reapply got status=%d errors=%v", res.status, res.Errors)
	}
	if res.Summary == nil || res.Summary.Noop != 1 {
		t.Fatalf("reapply summary = %+v, want one noop", res.Summary)
	}
}

// A variable error in a state apply leaves the state file untouched.
func TestStateApplyVariableErrorLeavesFileUntouched(t *testing.T) {
	path := stateFilePath(t)
	provider := &recordingProvider{}
	handler := HandlerWithProviderAndStateFile(provider, path)
	body := `{
	  "configuration": {
	    "resourceTypes": [{"name": "t", "schema": {"type": "object", "additionalProperties": true}}],
	    "variables": {},
	    "resources": [{"address": "a", "type": "t", "properties": {"n": {"$variable": "x"}}}]
	  }
	}`
	res := doStateApply(t, handler, body)
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (errors %v)", res.status, res.Errors)
	}
	if len(provider.calls) != 0 {
		t.Fatalf("provider must not be called, got %d calls", len(provider.calls))
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("state file must not exist after a variable error, stat err = %v", err)
	}
}
