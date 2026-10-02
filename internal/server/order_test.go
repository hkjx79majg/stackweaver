package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type orderResult struct {
	status  int
	Valid   bool              `json:"valid"`
	Errors  []validationError `json:"errors"`
	Order   *[]string         `json:"order"`
	headers http.Header
	raw     string
}

func postOrder(t *testing.T, body string) orderResult {
	t.Helper()
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/configurations/order", strings.NewReader(body)))
	return parseOrderResult(t, rec)
}

func parseOrderResult(t *testing.T, rec *httptest.ResponseRecorder) orderResult {
	t.Helper()
	var res orderResult
	res.status = rec.Code
	res.headers = rec.Header()
	res.raw = rec.Body.String()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("response is not JSON: %v (body %q)", err, rec.Body.String())
	}
	return res
}

const orderBaseTypes = `"resourceTypes": [
  {"name": "vm", "schema": {"type": "object"}}
]`

func orderResource(addr string, deps ...string) string {
	parts := []string{`"address": "` + addr + `"`, `"type": "vm"`, `"properties": {}`}
	if deps != nil {
		quoted := make([]string, len(deps))
		for i, d := range deps {
			quoted[i] = `"` + d + `"`
		}
		parts = append(parts, `"dependsOn": [`+strings.Join(quoted, ", ")+`]`)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func orderDoc(resources ...string) string {
	return "{" + orderBaseTypes + `, "resources": [` + strings.Join(resources, ", ") + "]}"
}

func TestOrderEmptyResources(t *testing.T) {
	res := postOrder(t, `{"resourceTypes": [], "resources": []}`)
	if res.status != http.StatusOK || !res.Valid || len(res.Errors) != 0 {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
	if res.Order == nil {
		t.Fatal("success response must include an order field")
	}
	if len(*res.Order) != 0 {
		t.Fatalf("order = %v, want empty array", *res.Order)
	}
	if !strings.Contains(res.raw, `"order":[]`) {
		t.Fatalf("empty order must serialize as [], body=%s", res.raw)
	}
}

func TestOrderTopologicalWithTieBreak(t *testing.T) {
	// d depends on b and c; b depends on a. Ready set after a: {b, c};
	// smallest first, then d last.
	body := orderDoc(
		orderResource("r.d", "r.b", "r.c"),
		orderResource("r.b", "r.a"),
		orderResource("r.a"),
		orderResource("r.c", "r.a"),
	)
	res := postOrder(t, body)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d errors=%v", res.status, res.Errors)
	}
	want := []string{"r.a", "r.b", "r.c", "r.d"}
	if len(*res.Order) != len(want) {
		t.Fatalf("order = %v, want %v", *res.Order, want)
	}
	for i := range want {
		if (*res.Order)[i] != want[i] {
			t.Fatalf("order = %v, want %v", *res.Order, want)
		}
	}
}

func TestOrderEquivalentDependsOnPermutations(t *testing.T) {
	// Swapping the order of references within dependsOn must not change
	// the resulting topological order.
	body1 := orderDoc(
		orderResource("r.z", "r.x", "r.y"),
		orderResource("r.y", "r.x"),
		orderResource("r.x"),
	)
	body2 := orderDoc(
		orderResource("r.z", "r.y", "r.x"),
		orderResource("r.y", "r.x"),
		orderResource("r.x"),
	)
	r1 := postOrder(t, body1)
	r2 := postOrder(t, body2)
	if r1.status != http.StatusOK || r2.status != http.StatusOK {
		t.Fatalf("statuses %d %d", r1.status, r2.status)
	}
	if strings.Join(*r1.Order, ",") != strings.Join(*r2.Order, ",") {
		t.Fatalf("orders differ: %v vs %v", *r1.Order, *r2.Order)
	}
	want := []string{"r.x", "r.y", "r.z"}
	for i := range want {
		if (*r1.Order)[i] != want[i] {
			t.Fatalf("order = %v, want %v", *r1.Order, want)
		}
	}
}

func TestOrderUnicodeTieBreak(t *testing.T) {
	// Independent resources: emitted by Unicode code point, not byte-safe
	// collation or input order. U+00E2 (â) < U+4E2D (中) < U+1F600 (😀).
	body := orderDoc(
		orderResource("r.😀"),
		orderResource("r.中"),
		orderResource("r.â"),
	)
	res := postOrder(t, body)
	if res.status != http.StatusOK {
		t.Fatalf("status=%d errors=%v", res.status, res.Errors)
	}
	want := []string{"r.â", "r.中", "r.😀"}
	for i := range want {
		if (*res.Order)[i] != want[i] {
			t.Fatalf("order = %v, want %v", *res.Order, want)
		}
	}
}

func TestOrderInvalidBaseDocumentReused(t *testing.T) {
	// Base validation failures use the validate contract and must not be
	// augmented with dependency errors, even though dependsOn is broken too.
	body := `{
	  "resourceTypes": [],
	  "resources": [
	    {"address": "a", "type": "ghost", "properties": {}, "dependsOn": "nope"}
	  ]
	}`
	res := postOrder(t, body)
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", res.status)
	}
	want := []string{"unknown_resource_type@/resources/0/type"}
	got := errCodes(orderAsValidate(res))
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("got %v, want %v", got, want)
	}
	if strings.Contains(res.raw, "order") {
		t.Fatalf("failure response must not contain order: %s", res.raw)
	}
}

func orderAsValidate(res orderResult) validateResult {
	return validateResult{Valid: res.Valid, Errors: res.Errors}
}

func TestOrderDependsOnShapeErrors(t *testing.T) {
	body := `{
	  "resourceTypes": [{"name": "vm", "schema": {"type": "object"}}],
	  "resources": [
	    {"address": "a", "type": "vm", "properties": {}, "dependsOn": "notarray"},
	    {"address": "b", "type": "vm", "properties": {}, "dependsOn": ["a", 3, "", "a", "missing", "b"]}
	  ]
	}`
	res := postOrder(t, body)
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", res.status)
	}
	want := []string{
		"invalid_document@/resources/0/dependsOn",
		"invalid_document@/resources/1/dependsOn/1",
		"invalid_document@/resources/1/dependsOn/2",
		"duplicate_dependency@/resources/1/dependsOn/3",
		"unknown_dependency@/resources/1/dependsOn/4",
		"self_dependency@/resources/1/dependsOn/5",
	}
	got := errCodes(orderAsValidate(res))
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// Response is sorted by path, then by code.
	sorted := append([]string(nil), got...)
	for i := 1; i < len(sorted); i++ {
		path := func(s string) string { return strings.SplitN(s, "@", 2)[1] }
		code := func(s string) string { return strings.SplitN(s, "@", 2)[0] }
		pi, pj := path(sorted[i-1]), path(sorted[i])
		if pi > pj || (pi == pj && code(sorted[i-1]) > code(sorted[i])) {
			t.Fatalf("errors not sorted by path then code: %v", sorted)
		}
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if strings.Contains(res.raw, "order") {
		t.Fatalf("failure response must not contain order: %s", res.raw)
	}
}

func TestOrderReferenceErrorsSuppressCycleDetection(t *testing.T) {
	// a <-> b form a cycle, but b also names an unknown address. Reference
	// errors must be reported without a dependency_cycle finding.
	body := `{
	  "resourceTypes": [{"name": "vm", "schema": {"type": "object"}}],
	  "resources": [
	    {"address": "a", "type": "vm", "properties": {}, "dependsOn": ["b"]},
	    {"address": "b", "type": "vm", "properties": {}, "dependsOn": ["a", "ghost"]}
	  ]
	}`
	res := postOrder(t, body)
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", res.status)
	}
	got := errCodes(orderAsValidate(res))
	if len(got) != 1 || got[0] != "unknown_dependency@/resources/1/dependsOn/1" {
		t.Fatalf("got %v, want only the unknown_dependency finding", got)
	}
}

func TestOrderSimpleCycle(t *testing.T) {
	body := orderDoc(
		orderResource("a", "b"),
		orderResource("b", "a"),
	)
	res := postOrder(t, body)
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", res.status)
	}
	if res.Valid || len(res.Errors) != 1 {
		t.Fatalf("payload = %+v", res)
	}
	e := res.Errors[0]
	if e.Code != "dependency_cycle" || e.Path != "/resources" || e.Message != "dependency cycle: a, b" {
		t.Fatalf("unexpected error: %+v", e)
	}
	if strings.Contains(res.raw, "order") {
		t.Fatalf("failure response must not contain order: %s", res.raw)
	}
}

func TestOrderCycleExcludesIndirectDependents(t *testing.T) {
	// c <-> d form the cycle; a depends on b, b depends on c (mere
	// predecessors of the cycle) and neither belongs in the cycle list.
	body := orderDoc(
		orderResource("a", "b"),
		orderResource("b", "c"),
		orderResource("c", "d"),
		orderResource("d", "c"),
	)
	res := postOrder(t, body)
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != "dependency_cycle" {
		t.Fatalf("errors = %v", res.Errors)
	}
	if msg := res.Errors[0].Message; msg != "dependency cycle: c, d" {
		t.Fatalf("message = %q, want %q", msg, "dependency cycle: c, d")
	}
}

func TestOrderMultipleDisjointCycles(t *testing.T) {
	// Two disjoint cycles: (b, a) and (e<->f); x feeds the second cycle.
	// All genuinely cyclic addresses appear, Unicode-sorted, joined by ", ".
	body := orderDoc(
		orderResource("a", "b"),
		orderResource("b", "a"),
		orderResource("x", "e"),
		orderResource("e", "f"),
		orderResource("f", "e"),
	)
	res := postOrder(t, body)
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", res.status)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("errors = %v, want exactly one", res.Errors)
	}
	e := res.Errors[0]
	if e.Code != "dependency_cycle" || e.Path != "/resources" {
		t.Fatalf("unexpected error: %+v", e)
	}
	if e.Message != "dependency cycle: a, b, e, f" {
		t.Fatalf("message = %q", e.Message)
	}
}

func TestOrderMalformedJSON(t *testing.T) {
	for name, body := range map[string]string{
		"empty":         "",
		"whitespace":    "   ",
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

func TestOrderMethodNotAllowed(t *testing.T) {
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
			t.Fatalf("%s: body not JSON: %v", method, err)
		}
		errObj, _ := payload["error"].(map[string]any)
		if errObj["code"] != "method_not_allowed" {
			t.Fatalf("%s: body = %v", method, payload)
		}
	}
}

func TestOrderIsStateless(t *testing.T) {
	// Ordering must not mutate or persist anything: repeated calls with the
	// same body return identical results.
	body := orderDoc(
		orderResource("b", "a"),
		orderResource("a"),
	)
	first := postOrder(t, body)
	second := postOrder(t, body)
	if first.raw != second.raw {
		t.Fatalf("responses differ:\n%s\n%s", first.raw, second.raw)
	}
}
