package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type orderResult struct {
	status int
	Valid  bool              `json:"valid"`
	Errors []validationError `json:"errors"`
	Order  *[]string         `json:"order"`
}

func postOrder(t *testing.T, body string) orderResult {
	t.Helper()
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/configurations/order", strings.NewReader(body)))
	var res orderResult
	res.status = rec.Code
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("response is not JSON: %v (body %q)", err, rec.Body.String())
	}
	return res
}

// orderBody wraps a resources fragment in a minimally valid document.
func orderErrCodes(res orderResult) []string {
	codes := make([]string, 0, len(res.Errors))
	for _, e := range res.Errors {
		codes = append(codes, e.Code+"@"+e.Path)
	}
	return codes
}

func orderBody(resources string) string {
	return `{
	  "resourceTypes": [{"name": "t", "schema": {"type": "object"}}],
	  "resources": ` + resources + `
	}`
}

func resEntry(address, dependsOn string) string {
	entry := `{"address": "` + address + `", "type": "t", "properties": {}`
	if dependsOn != "" {
		entry += `, "dependsOn": ` + dependsOn
	}
	return entry + `}`
}

func TestOrderEmptyResources(t *testing.T) {
	res := postOrder(t, `{"resourceTypes": [], "resources": []}`)
	if res.status != http.StatusOK || !res.Valid || len(res.Errors) != 0 {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
	if res.Order == nil {
		t.Fatal("order must be present")
	}
	if len(*res.Order) != 0 {
		t.Fatalf("order = %v, want []", *res.Order)
	}
}

func TestOrderWithoutDependsOnPreservesDocument(t *testing.T) {
	body := orderBody(`[` + resEntry("a", "") + `,` + resEntry("b", "") + `]`)
	res := postOrder(t, body)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d errors=%v", res.status, res.Errors)
	}
	got := *res.Order
	want := []string{"a", "b"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestOrderTopologicalWithTieBreak(t *testing.T) {
	// a depends on b and c; both depend on d. d must come first; when b and
	// c are both available, the smaller address wins; a comes last.
	body := orderBody(`[` +
		resEntry("a", `["b", "c"]`) + `,` +
		resEntry("b", `["d"]`) + `,` +
		resEntry("c", `["d"]`) + `,` +
		resEntry("d", `[]`) + `]`)
	res := postOrder(t, body)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d errors=%v", res.status, res.Errors)
	}
	want := []string{"d", "b", "c", "a"}
	if got := *res.Order; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestOrderDeterministicAcrossPermutations(t *testing.T) {
	documents := []string{
		orderBody(`[` +
			resEntry("a", `["c", "b"]`) + `,` +
			resEntry("b", `["d"]`) + `,` +
			resEntry("c", `["d"]`) + `,` +
			resEntry("d", `[]`) + `]`),
		orderBody(`[` +
			resEntry("d", `[]`) + `,` +
			resEntry("c", `["d"]`) + `,` +
			resEntry("b", `["d"]`) + `,` +
			resEntry("a", `["b", "c"]`) + `]`),
		orderBody(`[` +
			resEntry("c", `["d"]`) + `,` +
			resEntry("d", ``) + `,` +
			resEntry("a", `["c", "b"]`) + `,` +
			resEntry("b", `["d"]`) + `]`),
	}
	var first []string
	for i, body := range documents {
		res := postOrder(t, body)
		if res.status != http.StatusOK || !res.Valid {
			t.Fatalf("doc %d: got status=%d errors=%v", i, res.status, res.Errors)
		}
		if i == 0 {
			first = *res.Order
			continue
		}
		if strings.Join(*res.Order, "|") != strings.Join(first, "|") {
			t.Fatalf("doc %d order = %v, want %v", i, *res.Order, first)
		}
	}
	want := []string{"d", "b", "c", "a"}
	if strings.Join(first, "|") != strings.Join(want, "|") {
		t.Fatalf("order = %v, want %v", first, want)
	}
}

func TestOrderUnicodeCodePointTieBreak(t *testing.T) {
	// "b" (U+0062) < "中" (U+4E2D); all three are independently available.
	body := orderBody(`[` +
		resEntry("中", `[]`) + `,` +
		resEntry("b", `[]`) + `,` +
		resEntry("a", `[]`) + `]`)
	res := postOrder(t, body)
	if res.status != http.StatusOK {
		t.Fatalf("got status=%d errors=%v", res.status, res.Errors)
	}
	want := []string{"a", "b", "中"}
	if got := *res.Order; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func TestOrderDependsOnShapeErrors(t *testing.T) {
	body := orderBody(`[
	  ` + resEntry("a", `{}`) + `,
	  ` + resEntry("b", `["a", 3, "", "a"]`) + `
	]`)
	res := postOrder(t, body)
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", res.status)
	}
	want := []string{
		"invalid_document@/resources/0/dependsOn",
		"invalid_document@/resources/1/dependsOn/1",
		"invalid_document@/resources/1/dependsOn/2",
		"duplicate_dependency@/resources/1/dependsOn/3",
	}
	got := orderErrCodes(res)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestOrderUnknownAndSelfDependencies(t *testing.T) {
	body := orderBody(`[
	  ` + resEntry("a", `["a"]`) + `,
	  ` + resEntry("b", `["ghost", "a"]`) + `
	]`)
	res := postOrder(t, body)
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", res.status)
	}
	want := []string{
		"self_dependency@/resources/0/dependsOn/0",
		"unknown_dependency@/resources/1/dependsOn/0",
	}
	got := orderErrCodes(res)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestOrderErrorsSortedByPathThenCode(t *testing.T) {
	// Findings across resources must be ordered by JSON Pointer path
	// (string comparison, so /resources/0 precedes /resources/10); the
	// repeated self reference is a duplicate at its second occurrence.
	body := orderBody(`[
	  ` + resEntry("z", `["z"]`) + `,
	  ` + resEntry("a", `["nope"]`) + `,
	  ` + resEntry("m", `["m", "m"]`) + `
	]`)
	res := postOrder(t, body)
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", res.status)
	}
	want := []string{
		"self_dependency@/resources/0/dependsOn/0",
		"unknown_dependency@/resources/1/dependsOn/0",
		"self_dependency@/resources/2/dependsOn/0",
		"duplicate_dependency@/resources/2/dependsOn/1",
	}
	got := orderErrCodes(res)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestOrderDependencyErrorsSuppressCycle(t *testing.T) {
	// a <-> b form a cycle, but a also names an unknown address: the unknown
	// dependency is reported and no cycle is detected.
	body := orderBody(`[
	  ` + resEntry("a", `["b", "ghost"]`) + `,
	  ` + resEntry("b", `["a"]`) + `
	]`)
	res := postOrder(t, body)
	want := []string{"unknown_dependency@/resources/0/dependsOn/1"}
	got := orderErrCodes(res)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestOrderCycleReportsOnlyCyclicNodes(t *testing.T) {
	// a <-> m are on the cycle; x only leads into it and must be omitted.
	body := orderBody(`[
	  ` + resEntry("x", `["a"]`) + `,
	  ` + resEntry("a", `["m"]`) + `,
	  ` + resEntry("m", `["a"]`) + `
	]`)
	res := postOrder(t, body)
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", res.status)
	}
	if res.Valid || len(res.Errors) != 1 {
		t.Fatalf("unexpected payload: %+v", res)
	}
	e := res.Errors[0]
	if e.Code != "dependency_cycle" || e.Path != "/resources" {
		t.Fatalf("unexpected error: %+v", e)
	}
	if e.Message != "dependency cycle: a, m" {
		t.Fatalf("message = %q, want %q", e.Message, "dependency cycle: a, m")
	}
}

func TestOrderThreeNodeCycle(t *testing.T) {
	body := orderBody(`[
	  ` + resEntry("c", `["a"]`) + `,
	  ` + resEntry("a", `["b"]`) + `,
	  ` + resEntry("b", `["c"]`) + `,
	  ` + resEntry("d", `["a"]`) + `
	]`)
	res := postOrder(t, body)
	if len(res.Errors) != 1 || res.Errors[0].Code != "dependency_cycle" {
		t.Fatalf("got %v, want one dependency_cycle", res.Errors)
	}
	if msg := res.Errors[0].Message; msg != "dependency cycle: a, b, c" {
		t.Fatalf("message = %q", msg)
	}
}

func TestOrderBaseFailureSuppressesDependencyErrors(t *testing.T) {
	// Base validation fails (unknown type); the malformed dependsOn must not
	// add any dependency findings.
	body := `{
	  "resourceTypes": [],
	  "resources": [
	    {"address": "a", "type": "ghost", "properties": {}, "dependsOn": {}},
	    {"address": "a", "type": "ghost", "properties": {}, "dependsOn": ["nope"]}
	  ]
	}`
	res := postOrder(t, body)
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", res.status)
	}
	want := []string{
		"unknown_resource_type@/resources/0/type",
		"duplicate_identifier@/resources/1/address",
		"unknown_resource_type@/resources/1/type",
	}
	got := orderErrCodes(res)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestOrderMalformedJSON(t *testing.T) {
	for name, body := range map[string]string{
		"empty":         "",
		"broken":        `{`,
		"trailingValue": `{"resourceTypes": [], "resources": []} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			res := postOrder(t, body)
			if res.status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", res.status)
			}
			if res.Valid || len(res.Errors) != 1 || res.Errors[0].Code != "invalid_json" {
				t.Fatalf("unexpected payload: %+v", res)
			}
		})
	}
}

func TestOrderRejectsOtherMethods(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, httptest.NewRequest(method, "/v1/configurations/order", nil))
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

func TestOrderFailureResponsesOmitOrder(t *testing.T) {
	cases := map[string]string{
		"base":    `{"resourceTypes": [], "resources": [{"address":"a","type":"x","properties":{}}]}`,
		"dep":     orderBody(`[` + resEntry("a", `["ghost"]`) + `]`),
		"cycle":   orderBody(`[` + resEntry("a", `["b"]`) + `,` + resEntry("b", `["a"]`) + `]`),
		"invalid": `{`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/configurations/order", strings.NewReader(body)))
			var payload map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatalf("body is not JSON: %v", err)
			}
			if _, present := payload["order"]; present {
				t.Fatalf("failure response must not contain order: %v", payload)
			}
		})
	}
}

func TestOrderValidateEndpointAcceptsDependsOn(t *testing.T) {
	body := orderBody(`[` + resEntry("a", `["b"]`) + `,` + resEntry("b", `[]`) + `]`)
	res := postValidate(t, body)
	if res.status != http.StatusOK || !res.Valid || len(res.Errors) != 0 {
		t.Fatalf("validate got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
}
