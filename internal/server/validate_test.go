package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const endpoint = "/v1/configurations/validate"

func postValidate(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
	Handler().ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) validationResponse {
	t.Helper()
	var resp validationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not JSON: %v; body=%s", err, rec.Body.String())
	}
	return resp
}

func codes(resp validationResponse) map[string]bool {
	set := make(map[string]bool)
	for _, e := range resp.Errors {
		set[e.Code] = true
	}
	return set
}

func findError(resp validationResponse, path, code string) *validationError {
	for i := range resp.Errors {
		if resp.Errors[i].Path == path && resp.Errors[i].Code == code {
			return &resp.Errors[i]
		}
	}
	return nil
}

func TestValidateAcceptsEmptyArrays(t *testing.T) {
	rec := postValidate(t, `{"resourceTypes":[],"resources":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	resp := decode(t, rec)
	if !resp.Valid || len(resp.Errors) != 0 {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestValidateAcceptsNestedDocument(t *testing.T) {
	doc := `{
		"resourceTypes": [
			{"name": "svc", "schema": {"type": "object",
				"properties": {
					"port": {"type": "integer"},
					"ratio": {"type": "number"},
					"tags": {"type": "array", "items": {"type": "string"}},
					"meta": {"type": "object", "properties": {"env": {"type": "string"}},
						"required": ["env"], "additionalProperties": true}
				},
				"required": ["port", "meta"]}}
		],
		"resources": [
			{"address": "a/b.svc", "type": "svc", "properties": {
				"port": 8080, "ratio": 1.5, "tags": ["x", "y"],
				"meta": {"env": "prod", "extra": 1}}}
		]
	}`
	rec := postValidate(t, doc)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestValidateIntegerVsNumber(t *testing.T) {
	doc := `{
		"resourceTypes": [{"name": "t", "schema": {"type": "object", "properties": {
			"i": {"type": "integer"}, "n": {"type": "number"}},
			"additionalProperties": false}}],
		"resources": [{"address": "r", "type": "t", "properties": {"i": 1.0, "n": 2}}]
	}`
	rec := postValidate(t, doc)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	resp := decode(t, rec)
	if resp.Valid || findError(resp, "/resources/0/properties/i", codeTypeMismatch) == nil {
		t.Fatalf("integer should reject 1.0: %+v", resp)
	}
	// Integer token is a valid number.
	if e := findError(resp, "/resources/0/properties/n", codeTypeMismatch); e != nil {
		t.Fatalf("number should accept integer token 2: %+v", e)
	}
}

func TestValidateIntegerTokenForms(t *testing.T) {
	doc := `{
		"resourceTypes": [{"name": "t", "schema": {"type": "object", "properties": {
			"i": {"type": "integer"}, "n": {"type": "number"}},
			"additionalProperties": false}}],
		"resources": [{"address": "r", "type": "t", "properties":
			{"i": 900719925474099312345, "n": 1e2}}]
	}`
	rec := postValidate(t, doc)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	bad := `{
		"resourceTypes": [{"name": "t", "schema": {"type": "object", "properties": {
			"i2": {"type": "integer"}}, "additionalProperties": false}}],
		"resources": [{"address": "r", "type": "t", "properties": {"i2": 1e2}}]
	}`
	resp := decode(t, postValidate(t, bad))
	if findError(resp, "/resources/0/properties/i2", codeTypeMismatch) == nil {
		t.Fatalf("integer must reject exponent token 1e2: %+v", resp)
	}
}

func TestValidateNoImplicitConversion(t *testing.T) {
	doc := `{
		"resourceTypes": [{"name": "t", "schema": {"type": "object", "properties": {
			"s": {"type": "string"}, "b": {"type": "boolean"},
			"a": {"type": "array", "items": {"type": "string"}},
			"o": {"type": "object", "properties": {}}},
			"additionalProperties": false}}],
		"resources": [{"address": "r", "type": "t", "properties":
			{"s": 1, "b": "true", "a": "x", "o": []}}]
	}`
	rec := postValidate(t, doc)
	resp := decode(t, rec)
	for _, p := range []string{
		"/resources/0/properties/s",
		"/resources/0/properties/b",
		"/resources/0/properties/a",
		"/resources/0/properties/o",
	} {
		if findError(resp, p, codeTypeMismatch) == nil {
			t.Fatalf("expected type_mismatch at %s: %+v", p, resp)
		}
	}
}

func TestValidateRequiredAndUnexpectedProperties(t *testing.T) {
	doc := `{
		"resourceTypes": [{"name": "t", "schema": {"type": "object",
			"properties": {"keep": {"type": "string"}}, "required": ["keep"]}}],
		"resources": [{"address": "r", "type": "t", "properties": {"extra": 1}}]
	}`
	rec := postValidate(t, doc)
	resp := decode(t, rec)
	if findError(resp, "/resources/0/properties/keep", codeMissingRequired) == nil {
		t.Fatalf("expected missing_required_property for keep: %+v", resp)
	}
	if findError(resp, "/resources/0/properties/extra", codeUnexpectedProperty) == nil {
		t.Fatalf("expected unexpected_property for extra: %+v", resp)
	}
}

func TestValidateAdditionalPropertiesTrue(t *testing.T) {
	doc := `{
		"resourceTypes": [{"name": "t", "schema": {"type": "object",
			"properties": {"a": {"type": "string"}}, "additionalProperties": true}}],
		"resources": [{"address": "r", "type": "t", "properties": {"a": "x", "free": 42}}]
	}`
	rec := postValidate(t, doc)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
}

func TestValidateDuplicatesLocateLaterOccurrence(t *testing.T) {
	doc := `{
		"resourceTypes": [
			{"name": "dup", "schema": {"type": "string"}},
			{"name": "dup", "schema": {"type": "string"}}
		],
		"resources": [
			{"address": "x", "type": "dup", "properties": 123},
			{"address": "x", "type": "nope", "properties": 1}
		]
	}`
	rec := postValidate(t, doc)
	resp := decode(t, rec)
	if findError(resp, "/resourceTypes/1/name", codeDuplicateIdentifier) == nil {
		t.Fatalf("duplicate type must point at the later entry: %+v", resp)
	}
	if findError(resp, "/resources/1/address", codeDuplicateIdentifier) == nil {
		t.Fatalf("duplicate address must point at the later entry: %+v", resp)
	}
	// Duplicate/illegal types suppress property-level errors and surface as
	// unknown_resource_type for referencing resources.
	if findError(resp, "/resources/0/type", codeUnknownResourceType) == nil {
		t.Fatalf("duplicate type reference must be unknown_resource_type: %+v", resp)
	}
	if findError(resp, "/resources/0/properties", codeTypeMismatch) != nil {
		t.Fatalf("illegal/duplicate type must not trigger property errors: %+v", resp)
	}
}

func TestValidateUnknownTypeAndEmptyIdentifiers(t *testing.T) {
	doc := `{
		"resourceTypes": [{"name": "", "schema": {"type": "string"}}],
		"resources": [{"address": "", "type": "missing", "properties": {}}]
	}`
	rec := postValidate(t, doc)
	resp := decode(t, rec)
	if !codes(resp)[codeInvalidDocument] {
		t.Fatalf("expected invalid_document for empty name/address: %+v", resp)
	}
	if findError(resp, "/resources/0/type", codeUnknownResourceType) == nil {
		t.Fatalf("expected unknown_resource_type: %+v", resp)
	}
}

func TestValidateIllegalSchemaIsInvalidDocument(t *testing.T) {
	doc := `{
		"resourceTypes": [{"name": "t", "schema": {"type": "object",
			"properties": {"bad": {"type": "array"}},
			"required": ["port"], "additionalProperties": false}}],
		"resources": [{"address": "r", "type": "t", "properties": {"bad": []}}]
	}`
	rec := postValidate(t, doc)
	resp := decode(t, rec)
	if findError(resp, "/resourceTypes/0/schema/properties/bad", codeInvalidDocument) == nil {
		t.Fatalf("array schema without items must be invalid_document: %+v", resp)
	}
	// A resource referencing the structurally-invalid type gets
	// unknown_resource_type and no property-level errors.
	if findError(resp, "/resources/0/type", codeUnknownResourceType) == nil {
		t.Fatalf("invalid schema must make the type unusable: %+v", resp)
	}
}

func TestValidateRootStructure(t *testing.T) {
	for _, body := range []string{
		`{"resources": []}`,
		`{"resourceTypes": []}`,
		`{"resourceTypes": [], "resources": [], "nope": 1}`,
		`[1, 2]`,
		`"hello"`,
	} {
		rec := postValidate(t, body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("body %q: status = %d, want 422", body, rec.Code)
		}
		resp := decode(t, rec)
		if resp.Valid {
			t.Fatalf("body %q unexpectedly valid", body)
		}
	}
}

func TestValidateErrorsSortedByPathThenCode(t *testing.T) {
	doc := `{
		"resourceTypes": [{"name": "t", "schema": {"type": "object",
			"properties": {"a": {"type": "string"}, "b": {"type": "integer"}},
			"additionalProperties": false}}],
		"resources": [
			{"address": "r1", "type": "t", "properties": {"a": 1, "b": 2.5, "x": 1}},
			{"address": "r2", "type": "t", "properties": {"a": true}}
		]
	}`
	rec := postValidate(t, doc)
	resp := decode(t, rec)
	for i := 1; i < len(resp.Errors); i++ {
		prev, cur := resp.Errors[i-1], resp.Errors[i]
		if prev.Path > cur.Path || (prev.Path == cur.Path && prev.Code > cur.Code) {
			t.Fatalf("errors not sorted at %d: %+v before %+v", i, prev, cur)
		}
	}
}

func TestValidateBadJSON(t *testing.T) {
	for _, body := range []string{"", "   ", "{", `{"a":}`, `{"a":1} {"b":2}`, `[1] [2]`, `1 2`} {
		rec := postValidate(t, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q: status = %d, want 400; resp=%s", body, rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("body %q: content-type = %q", body, ct)
		}
		var payload errorBody
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("body %q: invalid JSON response: %v", body, err)
		}
		if payload.Error.Code != codeInvalidJSON {
			t.Fatalf("body %q: code = %q, want invalid_json", body, payload.Error.Code)
		}
	}
}

func TestValidateTrailingCommaAndWhitespaceAreFine(t *testing.T) {
	rec := postValidate(t, "  "+`{"resourceTypes":[],"resources":[]}`+"\n\t")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

func TestValidateMethodNotAllowed(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, httptest.NewRequest(method, endpoint, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
			t.Fatalf("%s: Allow = %q, want POST", method, allow)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("%s: content-type = %q", method, ct)
		}
	}
}

func TestValidateAlwaysJSONContentType(t *testing.T) {
	rec := postValidate(t, `{"resourceTypes":[],"resources":[]}`)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q", ct)
	}
}
