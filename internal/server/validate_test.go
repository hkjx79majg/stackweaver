package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type validateResult struct {
	status  int
	Valid   bool              `json:"valid"`
	Errors  []validationError `json:"errors"`
	headers http.Header
}

func postValidate(t *testing.T, body string) validateResult {
	t.Helper()
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/configurations/validate", strings.NewReader(body)))
	var res validateResult
	res.status = rec.Code
	res.headers = rec.Header()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("response is not JSON: %v (body %q)", err, rec.Body.String())
	}
	return res
}

func errCodes(res validateResult) []string {
	codes := make([]string, 0, len(res.Errors))
	for _, e := range res.Errors {
		codes = append(codes, e.Code+"@"+e.Path)
	}
	return codes
}

func TestValidateAcceptsValidDocument(t *testing.T) {
	body := `{
	  "resourceTypes": [
	    {"name": "net", "schema": {"type": "object",
	      "properties": {
	        "cidr": {"type": "string"},
	        "ports": {"type": "array", "items": {"type": "integer"}},
	        "tags": {"type": "object", "additionalProperties": true},
	        "nat": {"type": "object", "properties": {"enabled": {"type": "boolean"}}, "required": ["enabled"]}
	      },
	      "required": ["cidr"]}},
	    {"name": "vm", "schema": {"type": "object",
	      "properties": {"cpu": {"type": "integer"}, "weight": {"type": "number"}},
	      "required": ["cpu"], "additionalProperties": false}}
	  ],
	  "resources": [
	    {"address": "net.main", "type": "net", "properties": {
	      "cidr": "10.0.0.0/16", "ports": [80, 443], "tags": {"any": "thing"}, "nat": {"enabled": true}}},
	    {"address": "vm.web", "type": "vm", "properties": {"cpu": 4, "weight": 2.5}}
	  ]
	}`
	res := postValidate(t, body)
	if res.status != http.StatusOK || !res.Valid || len(res.Errors) != 0 {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
}

func TestValidateAcceptsEmptyArrays(t *testing.T) {
	res := postValidate(t, `{"resourceTypes": [], "resources": []}`)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
	if res.Errors == nil {
		t.Fatal("errors must be an empty array, not null")
	}
}

func TestValidateRejectsMalformedJSON(t *testing.T) {
	for name, body := range map[string]string{
		"empty":         "",
		"whitespace":    "  \n\t ",
		"broken":        `{"resourceTypes": [`,
		"trailingValue": `{"resourceTypes": [], "resources": []} {}`,
		"trailingJunk":  `{"resourceTypes": [], "resources": []} oops`,
	} {
		t.Run(name, func(t *testing.T) {
			res := postValidate(t, body)
			if res.status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", res.status)
			}
			if res.Valid || len(res.Errors) != 1 || res.Errors[0].Code != "invalid_json" {
				t.Fatalf("unexpected payload: %+v", res)
			}
		})
	}
}

func TestValidateRejectsOtherMethods(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, httptest.NewRequest(method, "/v1/configurations/validate", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
			t.Fatalf("%s: Allow = %q, want POST", method, allow)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("%s: Content-Type = %q, want application/json", method, ct)
		}
	}
}

func TestValidateDocumentStructure(t *testing.T) {
	cases := map[string]struct {
		body string
		want []string
	}{
		"root not object":     {`[]`, []string{"invalid_document@"}},
		"missing both":        {`{}`, []string{"invalid_document@", "invalid_document@"}},
		"missing types":       {`{"resources": []}`, []string{"invalid_document@"}},
		"missing resources":   {`{"resourceTypes": []}`, []string{"invalid_document@"}},
		"extra root field":    {`{"resourceTypes": [], "resources": [], "extra": 1}`, []string{`invalid_document@/extra`}},
		"types not array":     {`{"resourceTypes": {}, "resources": []}`, []string{"invalid_document@/resourceTypes"}},
		"resources not array": {`{"resourceTypes": [], "resources": {}}`, []string{"invalid_document@/resources"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			res := postValidate(t, tc.body)
			if res.status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422", res.status)
			}
			got := errCodes(res)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestValidateTypeEntryProblems(t *testing.T) {
	body := `{
	  "resourceTypes": [
	    {"name": "ok", "schema": {"type": "string"}},
	    {"schema": {"type": "string"}},
	    {"name": "", "schema": {"type": "string"}},
	    {"name": 7, "schema": {"type": "string"}},
	    {"name": "noschema"},
	    {"name": "badtype", "schema": {"type": "strange"}},
	    {"name": "baditems", "schema": {"type": "array"}},
	    {"name": "badnested", "schema": {"type": "object", "properties": {"p": {"type": "nope"}}}},
	    "not-an-object"
	  ],
	  "resources": []
	}`
	res := postValidate(t, body)
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", res.status)
	}
	want := []string{
		"invalid_document@/resourceTypes/1",
		"invalid_document@/resourceTypes/2/name",
		"invalid_document@/resourceTypes/3/name",
		"invalid_document@/resourceTypes/4",
		"invalid_document@/resourceTypes/5/schema/type",
		"invalid_document@/resourceTypes/6/schema",
		"invalid_document@/resourceTypes/7/schema/properties/p/type",
		"invalid_document@/resourceTypes/8",
	}
	got := errCodes(res)
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestValidateDuplicateIdentifiers(t *testing.T) {
	body := `{
	  "resourceTypes": [
	    {"name": "net", "schema": {"type": "object", "properties": {"cidr": {"type": "string"}}}},
	    {"name": "net", "schema": {"type": "object"}}
	  ],
	  "resources": [
	    {"address": "a.b", "type": "net", "properties": {"cidr": 5, "extra": true}},
	    {"address": "a.b", "type": "net", "properties": {}}
	  ]
	}`
	res := postValidate(t, body)
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", res.status)
	}
	// The duplicated type name is tainted: neither resource triggers
	// property-level errors. Only the later occurrences are flagged.
	want := []string{
		"duplicate_identifier@/resourceTypes/1/name",
		"duplicate_identifier@/resources/1/address",
	}
	got := errCodes(res)
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestValidateInvalidTypeDoesNotCascade(t *testing.T) {
	body := `{
	  "resourceTypes": [
	    {"name": "broken", "schema": {"type": "nope"}}
	  ],
	  "resources": [
	    {"address": "x.y", "type": "broken", "properties": {"anything": 1}}
	  ]
	}`
	res := postValidate(t, body)
	got := errCodes(res)
	want := []string{"invalid_document@/resourceTypes/0/schema/type"}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestValidateUnknownResourceType(t *testing.T) {
	body := `{
	  "resourceTypes": [{"name": "net", "schema": {"type": "object"}}],
	  "resources": [
	    {"address": "a.b", "type": "ghost", "properties": {"x": 1}},
	    {"address": "c.d", "properties": {}}
	  ]
	}`
	res := postValidate(t, body)
	want := []string{
		"unknown_resource_type@/resources/0/type",
		"invalid_document@/resources/1",
	}
	got := errCodes(res)
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestValidatePropertyChecks(t *testing.T) {
	body := `{
	  "resourceTypes": [
	    {"name": "vm", "schema": {"type": "object",
	      "properties": {
	        "cpu": {"type": "integer"},
	        "weight": {"type": "number"},
	        "name": {"type": "string"},
	        "on": {"type": "boolean"},
	        "ports": {"type": "array", "items": {"type": "integer"}},
	        "os": {"type": "object", "properties": {"family": {"type": "string"}}, "required": ["family"]}
	      },
	      "required": ["cpu", "name"]}}
	  ],
	  "resources": [
	    {"address": "vm.a", "type": "vm", "properties": {
	      "cpu": 2.5,
	      "weight": "heavy",
	      "on": 1,
	      "ports": [80, "ssl"],
	      "os": {"family": 9},
	      "surprise": null
	    }}
	  ]
	}`
	res := postValidate(t, body)
	want := []string{
		"missing_required_property@/resources/0/properties",
		"type_mismatch@/resources/0/properties/cpu",
		"type_mismatch@/resources/0/properties/on",
		"type_mismatch@/resources/0/properties/os/family",
		"type_mismatch@/resources/0/properties/ports/1",
		"unexpected_property@/resources/0/properties/surprise",
		"type_mismatch@/resources/0/properties/weight",
	}
	got := errCodes(res)
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestValidateNumberSemantics(t *testing.T) {
	body := `{
	  "resourceTypes": [
	    {"name": "n", "schema": {"type": "object", "properties": {
	      "i": {"type": "integer"}, "f": {"type": "number"}}}}
	  ],
	  "resources": [
	    {"address": "ok", "type": "n", "properties": {"i": 5, "f": 5}},
	    {"address": "ok2", "type": "n", "properties": {"i": 5, "f": 5.5}},
	    {"address": "bad", "type": "n", "properties": {"i": 5.5}},
	    {"address": "bad2", "type": "n", "properties": {"i": "5", "f": true}}
	  ]
	}`
	res := postValidate(t, body)
	want := []string{
		"type_mismatch@/resources/2/properties/i",
		"type_mismatch@/resources/3/properties/f",
		"type_mismatch@/resources/3/properties/i",
	}
	got := errCodes(res)
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestValidateAdditionalPropertiesDefaultFalse(t *testing.T) {
	body := `{
	  "resourceTypes": [
	    {"name": "closed", "schema": {"type": "object", "properties": {"a": {"type": "string"}}}},
	    {"name": "open", "schema": {"type": "object", "properties": {"a": {"type": "string"}}, "additionalProperties": true}}
	  ],
	  "resources": [
	    {"address": "r.closed", "type": "closed", "properties": {"a": "x", "b": 1}},
	    {"address": "r.open", "type": "open", "properties": {"a": "x", "b": 1}}
	  ]
	}`
	res := postValidate(t, body)
	want := []string{"unexpected_property@/resources/0/properties/b"}
	got := errCodes(res)
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestValidateErrorsSortedByPathThenCode(t *testing.T) {
	body := `{
	  "resourceTypes": [],
	  "resources": [
	    {"address": "z", "type": "ghost", "properties": {}},
	    {"address": "y", "type": "ghost", "properties": {}}
	  ],
	  "extra": 1
	}`
	res := postValidate(t, body)
	want := []string{
		"invalid_document@/extra",
		"unknown_resource_type@/resources/0/type",
		"unknown_resource_type@/resources/1/type",
	}
	got := errCodes(res)
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestValidateEmptyAddressAndType(t *testing.T) {
	body := `{
	  "resourceTypes": [],
	  "resources": [
	    {"address": "", "type": "", "properties": {}}
	  ]
	}`
	res := postValidate(t, body)
	want := []string{
		"invalid_document@/resources/0/address",
		"invalid_document@/resources/0/type",
	}
	got := errCodes(res)
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
