package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// reconcileProvider is a Provider + Observer driven by scripted snapshots,
// observation errors keyed by address, and an Apply failure at a given
// execution index. It records the exact order of both calls.
type reconcileProvider struct {
	snapshots   map[string]*Snapshot
	observeErrs map[string]error
	applyFailAt int
	applyErr    error
	observes    []string
	applies     []ChangeRequest
	lastObCtx   context.Context
	lastApCtx   context.Context
}

func (p *reconcileProvider) Apply(req ChangeRequest) error {
	p.applies = append(p.applies, req)
	p.lastApCtx = req.Context
	if p.applyErr != nil && len(p.applies)-1 == p.applyFailAt {
		return p.applyErr
	}
	return nil
}

func (p *reconcileProvider) Observe(ctx context.Context, address string) (*Snapshot, error) {
	p.observes = append(p.observes, address)
	p.lastObCtx = ctx
	if err := p.observeErrs[address]; err != nil {
		return nil, err
	}
	return p.snapshots[address], nil
}

type reconcileResult struct {
	status  int
	raw     map[string]any
	Valid   bool              `json:"valid"`
	Errors  []validationError `json:"errors"`
	Applied *[]planChange     `json:"applied"`
	Summary *planSummary      `json:"summary"`
	State   *struct {
		Resources []stateResource `json:"resources"`
	} `json:"state"`
	Revision        *int64 `json:"revision"`
	CurrentRevision *int64 `json:"currentRevision"`
}

func doReconcile(t *testing.T, handler http.Handler, body string) reconcileResult {
	t.Helper()
	return doReconcileRequest(t, handler, httptest.NewRequest(http.MethodPost, "/v1/state/reconcile", strings.NewReader(body)))
}

func doReconcileRequest(t *testing.T, handler http.Handler, req *http.Request) reconcileResult {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var res reconcileResult
	res.status = rec.Code
	if err := json.Unmarshal(rec.Body.Bytes(), &res.raw); err != nil {
		t.Fatalf("response is not JSON: %v (body %q)", err, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode typed response: %v (body %q)", err, rec.Body.String())
	}
	return res
}

func TestReconcileUnavailableWithoutStateFile(t *testing.T) {
	provider := &reconcileProvider{}
	for name, handler := range map[string]http.Handler{
		"Handler":             Handler(),
		"HandlerWithProvider": HandlerWithProvider(provider),
	} {
		res := doReconcile(t, handler, `{}`)
		if res.status != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d, want 503", name, res.status)
		}
		if res.Valid || len(res.Errors) != 1 || res.Errors[0].Code != codeStateUnavailable {
			t.Fatalf("%s: unexpected body %+v", name, res)
		}
		if len(provider.observes) != 0 || len(provider.applies) != 0 {
			t.Fatalf("%s: provider called without a state file", name)
		}
	}
}

func TestReconcileObserverUnavailable(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 3, `[]`)
	provider := &recordingProvider{}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doReconcile(t, handler, `{}`)
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.status)
	}
	if res.Valid || len(res.Errors) != 1 || res.Errors[0].Code != codeObserverUnavailable {
		t.Fatalf("unexpected body %+v", res)
	}
	if len(provider.calls) != 0 {
		t.Fatalf("provider called without an Observer: %v", provider.calls)
	}
}

func TestReconcileObserverUnavailableBeforeLock(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 3, `[]`)
	handler := HandlerWithProviderAndStateFile(&recordingProvider{}, path)

	// Even with the lock held, the missing Observer is reported first.
	unlock, ok := tryLockState(newStateBackend(path).path)
	if !ok {
		t.Fatal("could not take state lock")
	}
	defer unlock()

	res := doReconcile(t, handler, `{}`)
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeObserverUnavailable {
		t.Fatalf("unexpected body %+v", res)
	}
}

func TestReconcileRejectsOtherMethods(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 0, `[]`)
	handler := HandlerWithProviderAndStateFile(&reconcileProvider{}, path)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/v1/state/reconcile", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
			t.Fatalf("%s: Allow = %q, want POST", method, allow)
		}
	}
}

func TestReconcileInvalidJSONIs400(t *testing.T) {
	provider := &reconcileProvider{}
	handler := HandlerWithProviderAndStateFile(provider, stateFilePath(t))
	for _, body := range []string{"", `{`, `{} {}`} {
		res := doReconcile(t, handler, body)
		if res.status != http.StatusBadRequest {
			t.Fatalf("body %q: status = %d, want 400", body, res.status)
		}
		if res.Valid || len(res.Errors) != 1 || res.Errors[0].Code != codeInvalidJSON {
			t.Fatalf("body %q: payload = %+v", body, res)
		}
	}
	if len(provider.observes) != 0 || len(provider.applies) != 0 {
		t.Fatalf("provider called for malformed JSON")
	}
}

func TestReconcileEnvelopeErrorsAre422(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 2, `[]`)
	provider := &reconcileProvider{}
	handler := HandlerWithProviderAndStateFile(provider, path)
	cases := map[string]string{
		"not an object":       `[]`,
		"null":                `null`,
		"unknown field":       `{"expectedRevision": 2, "unexpected": true}`,
		"negative revision":   `{"expectedRevision": -1}`,
		"fractional revision": `{"expectedRevision": 2.5}`,
		"string revision":     `{"expectedRevision": "2"}`,
		"null revision":       `{"expectedRevision": null}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			res := doReconcile(t, handler, body)
			if res.status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (body %v)", res.status, res.raw)
			}
			if res.Valid || len(res.Errors) == 0 {
				t.Fatalf("payload = %+v", res)
			}
			for _, e := range res.Errors {
				if e.Code != codeInvalidReconcileRequest {
					t.Fatalf("error code = %q, want invalid_reconcile_request", e.Code)
				}
			}
		})
	}
	if len(provider.observes) != 0 || len(provider.applies) != 0 {
		t.Fatalf("provider called for an invalid envelope")
	}
}

func TestReconcileStateLocked(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 1, `[]`)
	provider := &reconcileProvider{}
	handler := HandlerWithProviderAndStateFile(provider, path)

	unlock, ok := tryLockState(newStateBackend(path).path)
	if !ok {
		t.Fatal("could not take state lock")
	}
	defer unlock()

	res := doReconcile(t, handler, `{}`)
	if res.status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeStateLocked {
		t.Fatalf("unexpected body %+v", res)
	}
	if len(provider.observes) != 0 || len(provider.applies) != 0 {
		t.Fatalf("provider called while locked")
	}
}

func TestReconcileStateReadError(t *testing.T) {
	path := stateFilePath(t)
	if err := os.WriteFile(path, []byte(`{"revision": "nope"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	provider := &reconcileProvider{}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doReconcile(t, handler, `{}`)
	if res.status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeStateReadError {
		t.Fatalf("unexpected body %+v", res)
	}
	if len(provider.observes) != 0 || len(provider.applies) != 0 {
		t.Fatalf("provider called for unreadable state")
	}
}

func TestReconcileExpectedRevisionConflict(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 3, `[{"address": "a", "type": "t", "properties": {}}]`)
	provider := &reconcileProvider{}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doReconcile(t, handler, `{"expectedRevision": 4}`)
	if res.status != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %v)", res.status, res.raw)
	}
	if res.Valid || len(res.Errors) != 1 || res.Errors[0].Code != codeStateConflict {
		t.Fatalf("errors = %+v, want state_conflict", res.Errors)
	}
	if res.CurrentRevision == nil || *res.CurrentRevision != 3 {
		t.Fatalf("currentRevision = %v, want 3", res.CurrentRevision)
	}
	if len(provider.observes) != 0 || len(provider.applies) != 0 {
		t.Fatalf("conflict must not call Observe or Apply")
	}
	if res.Revision != nil || res.State != nil || res.Applied != nil {
		t.Fatalf("conflict response must carry only currentRevision: %v", res.raw)
	}

	// A matching expectation proceeds.
	match := doReconcile(t, handler, `{"expectedRevision": 3}`)
	if match.status != http.StatusOK {
		t.Fatalf("matching run: status = %d body %v", match.status, match.raw)
	}

	// A missing file reads as revision 0.
	missingPath := stateFilePath(t)
	missingHandler := HandlerWithProviderAndStateFile(&reconcileProvider{}, missingPath)
	missing := doReconcile(t, missingHandler, `{"expectedRevision": 1}`)
	if missing.status != http.StatusConflict || missing.CurrentRevision == nil || *missing.CurrentRevision != 0 {
		t.Fatalf("missing file: status = %d currentRevision = %v", missing.status, missing.CurrentRevision)
	}
}

func TestReconcileEmptyState(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 7, `[]`)
	provider := &reconcileProvider{}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doReconcile(t, handler, `{}`)
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %v)", res.status, res.raw)
	}
	if !res.Valid || len(res.Errors) != 0 {
		t.Fatalf("unexpected valid/errors: %+v", res)
	}
	if res.Revision == nil || *res.Revision != 7 {
		t.Fatalf("revision = %v, want 7", res.Revision)
	}
	if res.Applied == nil || len(*res.Applied) != 0 {
		t.Fatalf("applied = %v, want empty array", res.Applied)
	}
	if res.Summary == nil || *res.Summary != (planSummary{}) {
		t.Fatalf("summary = %+v, want all zero (delete included)", res.Summary)
	}
	if res.State == nil || len(res.State.Resources) != 0 {
		t.Fatalf("state = %+v, want empty resources", res.State)
	}
	if len(provider.observes) != 0 || len(provider.applies) != 0 {
		t.Fatalf("Observer/Provider called for empty state")
	}
	if got := fileContent(t, path); !strings.Contains(got, `"revision": 7`) {
		t.Fatalf("state file changed: %s", got)
	}
}

func TestReconcileMissingFileReadsAsEmpty(t *testing.T) {
	path := stateFilePath(t)
	provider := &reconcileProvider{}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doReconcile(t, handler, `{}`)
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.status)
	}
	if res.Revision == nil || *res.Revision != 0 {
		t.Fatalf("revision = %v, want 0", res.Revision)
	}
	if len(provider.observes) != 0 {
		t.Fatalf("Observer called for empty state: %v", provider.observes)
	}
}

func TestReconcileAllNoopDoesNotApply(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 2, `[
	  {"address": "a", "type": "t", "properties": {"n": 1}},
	  {"address": "b", "type": "t", "properties": {}, "dependsOn": ["a"]}
	]`)
	provider := &reconcileProvider{snapshots: map[string]*Snapshot{
		"a": {Type: "t", Properties: map[string]any{"n": 1.0}},
		"b": {Type: "t", Properties: map[string]any{}, DependsOn: []string{"a"}},
	}}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doReconcile(t, handler, `{}`)
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.status)
	}
	if res.Summary == nil || *res.Summary != (planSummary{Noop: 2}) {
		t.Fatalf("summary = %+v, want noop=2 delete=0", res.Summary)
	}
	if res.Applied == nil || len(*res.Applied) != 0 {
		t.Fatalf("applied = %v, want empty", res.Applied)
	}
	if len(provider.applies) != 0 {
		t.Fatalf("Apply called for noops: %d", len(provider.applies))
	}
}

func TestReconcileClassifiesAndExecutesInTopologicalOrder(t *testing.T) {
	path := stateFilePath(t)
	original := `[
	  {"address": "c", "type": "vm", "properties": {"size": 2}},
	  {"address": "a", "type": "net", "properties": {"cidr": "10.0.0.0/8", "n": 1}},
	  {"address": "b", "type": "vm", "properties": {"size": 1}, "dependsOn": ["a"]}
	]`
	writeDriftStateFile(t, path, 4, original)
	provider := &reconcileProvider{snapshots: map[string]*Snapshot{
		// Equivalent: numeric spelling and key order differ.
		"a": {Type: "net", Properties: map[string]any{"n": 1.0, "cidr": "10.0.0.0/8"}},
		// Different: update.
		"b": {Type: "vm", Properties: map[string]any{"size": 3}},
		// Absent remotely: create.
	}}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doReconcile(t, handler, `{}`)
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %v)", res.status, res.raw)
	}
	if !res.Valid || len(res.Errors) != 0 {
		t.Fatalf("unexpected valid/errors: %+v", res)
	}
	if got, want := provider.observes, []string{"a", "b", "c"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("observe order = %v, want %v (Unicode address order)", got, want)
	}
	if res.Summary == nil {
		t.Fatal("missing summary")
	}
	if want := (planSummary{Create: 1, Update: 1, Delete: 0, Noop: 1}); *res.Summary != want {
		t.Fatalf("summary = %+v, want %+v", res.Summary, want)
	}

	// Saved topological order is a, b, c; only b and c change, so they apply
	// as update:b then create:c — dependencies converge first.
	if res.Applied == nil || len(*res.Applied) != 2 {
		t.Fatalf("applied = %+v, want 2 changes", res.Applied)
	}
	applied := *res.Applied
	if applied[0].Address != "b" || applied[0].Action != "update" {
		t.Fatalf("applied[0] = %+v, want update b", applied[0])
	}
	if applied[1].Address != "c" || applied[1].Action != "create" {
		t.Fatalf("applied[1] = %+v, want create c", applied[1])
	}

	// Update carries the observed snapshot as before and the saved snapshot
	// as after; create omits before and carries the saved snapshot as after.
	update := applied[0]
	if update.Before == nil || update.After == nil {
		t.Fatalf("update must carry both snapshots: %+v", update)
	}
	if got := update.Before.Properties["size"]; gotNumber(got) != 3 {
		t.Fatalf("update before = %v, want observed {size:3}", update.Before)
	}
	if got := update.After.Properties["size"]; gotNumber(got) != 1 {
		t.Fatalf("update after = %v, want saved {size:1}", update.After)
	}
	if got := update.After.DependsOn; len(got) != 1 || got[0] != "a" {
		t.Fatalf("update after.dependsOn = %v, want [a]", got)
	}
	create := applied[1]
	if create.Before != nil {
		t.Fatalf("create must omit before: %+v", create)
	}
	if create.After == nil || gotNumber(create.After.Properties["size"]) != 2 {
		t.Fatalf("create after = %v, want saved {size:2}", create.After)
	}

	if got := providerCallsSummary(provider.applies); got != "update:b|create:c" {
		t.Fatalf("Apply calls = %q, want update:b|create:c", got)
	}

	// The response carries the original saved state and revision.
	if res.Revision == nil || *res.Revision != 4 {
		t.Fatalf("revision = %v, want 4", res.Revision)
	}
	if res.State == nil || len(res.State.Resources) != 3 {
		t.Fatalf("state = %+v, want the saved state", res.State)
	}
	// The file itself keeps its revision; a GET then confirms its contents.
	if got := fileContent(t, path); !strings.Contains(got, `"revision": 4`) {
		t.Fatalf("state file revision changed:\n%s", got)
	}
	got := doStateGet(t, handler)
	if got.status != http.StatusOK || got.Revision != 4 || len(got.State.Resources) != 3 {
		t.Fatalf("GET after reconcile: status=%d revision=%d resources=%d, want 200/4/3",
			got.status, got.Revision, len(got.State.Resources))
	}
}

// gotNumber reads an int-like decoded JSON value in tests.
func gotNumber(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return -1
	}
}

func TestReconcileCreateOrderIsDependencyFirst(t *testing.T) {
	path := stateFilePath(t)
	// a depends on b; both are missing remotely.
	writeDriftStateFile(t, path, 1, `[
	  {"address": "a", "type": "t", "properties": {}, "dependsOn": ["b"]},
	  {"address": "b", "type": "t", "properties": {}}
	]`)
	provider := &reconcileProvider{}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doReconcile(t, handler, `{}`)
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.status)
	}
	if got, want := provider.observes, []string{"a", "b"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("observe order = %v, want %v", got, want)
	}
	if got := providerCallsSummary(provider.applies); got != "create:b|create:a" {
		t.Fatalf("Apply calls = %q, want create:b|create:a (dependency first)", got)
	}
	if res.Summary == nil || *res.Summary != (planSummary{Create: 2}) {
		t.Fatalf("summary = %+v, want create=2", res.Summary)
	}
}

func TestReconcileObserverErrorStopsBeforeApply(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 5, `[
	  {"address": "a", "type": "t", "properties": {}},
	  {"address": "b", "type": "t", "properties": {}},
	  {"address": "c", "type": "t", "properties": {}}
	]`)
	provider := &reconcileProvider{
		snapshots:   map[string]*Snapshot{"a": {Type: "t", Properties: map[string]any{}}},
		observeErrs: map[string]error{"b": errors.New("remote exploded")},
	}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doReconcile(t, handler, `{}`)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if res.Valid || len(res.Errors) != 1 {
		t.Fatalf("unexpected body %+v", res)
	}
	if res.Errors[0].Code != codeObserverError || res.Errors[0].Message != "remote exploded" {
		t.Fatalf("unexpected error %+v", res.Errors[0])
	}
	if res.Errors[0].Path != "/resources/1" {
		t.Fatalf("path = %q, want /resources/1", res.Errors[0].Path)
	}
	if res.Applied != nil {
		t.Fatalf("failure response must not carry applied: %+v", res.Applied)
	}
	if len(provider.applies) != 0 {
		t.Fatalf("Apply must not run after an observation error")
	}
	if got, want := provider.observes, []string{"a", "b"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("observe calls = %v, want %v (run must stop)", got, want)
	}
	if got := fileContent(t, path); !strings.Contains(got, `"revision": 5`) {
		t.Fatalf("state file changed: %s", got)
	}
}

func TestReconcileObserverInvalidResult(t *testing.T) {
	cases := map[string]*Snapshot{
		"empty type":       {Type: "", Properties: map[string]any{}},
		"nil properties":   {Type: "t", Properties: nil},
		"empty dependency": {Type: "t", Properties: map[string]any{}, DependsOn: []string{""}},
		"duplicate deps":   {Type: "t", Properties: map[string]any{}, DependsOn: []string{"x", "x"}},
		"unencodable":      {Type: "t", Properties: map[string]any{"bad": func() {}}},
	}
	for name, snapshot := range cases {
		t.Run(name, func(t *testing.T) {
			path := stateFilePath(t)
			writeDriftStateFile(t, path, 2, `[
			  {"address": "a", "type": "t", "properties": {}},
			  {"address": "b", "type": "t", "properties": {}}
			]`)
			provider := &reconcileProvider{snapshots: map[string]*Snapshot{"a": snapshot}}
			handler := HandlerWithProviderAndStateFile(provider, path)

			res := doReconcile(t, handler, `{}`)
			if res.status != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502", res.status)
			}
			if len(res.Errors) != 1 || res.Errors[0].Code != codeObserverInvalidResult {
				t.Fatalf("unexpected body %+v", res)
			}
			if res.Errors[0].Path != "/resources/0" {
				t.Fatalf("path = %q, want /resources/0", res.Errors[0].Path)
			}
			if res.Applied != nil || len(provider.applies) != 0 {
				t.Fatalf("invalid observation must not apply any change")
			}
		})
	}
}

func TestReconcileProviderFailureStopsWithoutRollback(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 9, `[
	  {"address": "a", "type": "t", "properties": {}, "dependsOn": ["b"]},
	  {"address": "b", "type": "t", "properties": {}}
	]`)
	provider := &reconcileProvider{applyFailAt: 1, applyErr: errors.New("boom at a")}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doReconcile(t, handler, `{}`)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if res.Valid || len(res.Errors) != 1 {
		t.Fatalf("unexpected body %+v", res)
	}
	e := res.Errors[0]
	if e.Code != codeProviderError || e.Message != "boom at a" || e.Path != "/changes/1" {
		t.Fatalf("error = %+v", e)
	}
	if got := providerCallsSummary(provider.applies); got != "create:b|create:a" {
		t.Fatalf("Apply calls = %q, want both attempted (second failed)", got)
	}
	if res.Applied == nil || len(*res.Applied) != 1 || (*res.Applied)[0].Address != "b" {
		t.Fatalf("applied = %v, want only b", res.Applied)
	}
	// The full original summary survives.
	if res.Summary == nil || *res.Summary != (planSummary{Create: 2}) {
		t.Fatalf("summary = %+v, want create=2", res.Summary)
	}
	if res.Revision == nil || *res.Revision != 9 {
		t.Fatalf("revision = %v, want 9 (never bumped)", res.Revision)
	}
	if got := fileContent(t, path); !strings.Contains(got, `"revision": 9`) {
		t.Fatalf("state file changed after provider failure: %s", got)
	}

	// No rollback and the lock is always released: an identical follow-up run
	// through a separately created handler reaches the provider again rather
	// than reporting state_locked.
	followUp := &reconcileProvider{applyFailAt: 1, applyErr: errors.New("boom at a")}
	again := doReconcile(t, HandlerWithProviderAndStateFile(followUp, path), `{}`)
	if again.status != http.StatusBadGateway {
		t.Fatalf("follow-up status = %d, want 502 again (not state_locked)", again.status)
	}
	if len(again.Errors) != 1 || again.Errors[0].Code != codeProviderError {
		t.Fatalf("follow-up errors = %+v, want provider_error", again.Errors)
	}
}

func TestReconcileNeverPersistsAcrossRequests(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 5, `[
	  {"address": "a", "type": "t", "properties": {}},
	  {"address": "b", "type": "t", "properties": {}}
	]`)
	provider := &reconcileProvider{}
	handler := HandlerWithProviderAndStateFile(provider, path)

	first := doReconcile(t, handler, `{}`)
	if first.status != http.StatusOK || first.Summary == nil || first.Summary.Create != 2 {
		t.Fatalf("first run: status = %d summary = %+v", first.status, first.Summary)
	}
	// The remote is still empty and nothing was persisted, so the second run
	// observes the same drift and creates both resources again.
	second := doReconcile(t, handler, `{}`)
	if second.status != http.StatusOK {
		t.Fatalf("second run: status = %d", second.status)
	}
	if second.Summary == nil || *second.Summary != (planSummary{Create: 2}) {
		t.Fatalf("second summary = %+v, want create=2 again", second.Summary)
	}
	if got := len(provider.applies); got != 4 {
		t.Fatalf("Apply calls = %d, want 4 across two independent runs", got)
	}
	rev, resources := readStateFileRaw(t, path)
	if rev != 5 || len(resources) != 2 {
		t.Fatalf("state file = revision %d with %d resources, want 5/2", rev, len(resources))
	}
}

func TestReconcilePassesRequestContext(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 1, `[{"address": "a", "type": "t", "properties": {}}]`)
	provider := &reconcileProvider{}
	handler := HandlerWithProviderAndStateFile(provider, path)

	type ctxKey struct{}
	req := httptest.NewRequest(http.MethodPost, "/v1/state/reconcile", strings.NewReader(`{}`))
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, "marker"))
	res := doReconcileRequest(t, handler, req)
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.status)
	}
	if provider.lastObCtx == nil || provider.lastObCtx.Value(ctxKey{}) != "marker" {
		t.Fatalf("Observer did not receive the request context unchanged")
	}
	if provider.lastApCtx == nil || provider.lastApCtx.Value(ctxKey{}) != "marker" {
		t.Fatalf("Apply did not receive the request context unchanged")
	}
}
