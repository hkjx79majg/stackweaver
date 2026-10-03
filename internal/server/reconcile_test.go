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

// reconcileProvider is a Provider and Observer driven by scripted snapshots
// and errors; it records the Observe order and every Apply request.
type reconcileProvider struct {
	snapshots    map[string]*Snapshot
	observeErrs  map[string]error
	observeCalls []string
	applyCalls   []ChangeRequest
	failAt       int
	failErr      error
	observeCtx   context.Context
	applyCtx     context.Context
}

func (p *reconcileProvider) Apply(req ChangeRequest) error {
	if p.applyCtx != nil && req.Context != p.applyCtx {
		return errors.New("apply did not carry the inbound context")
	}
	idx := len(p.applyCalls)
	p.applyCalls = append(p.applyCalls, req)
	if p.failErr != nil && idx == p.failAt {
		return p.failErr
	}
	return nil
}

func (p *reconcileProvider) Observe(ctx context.Context, address string) (*Snapshot, error) {
	p.observeCtx = ctx
	p.observeCalls = append(p.observeCalls, address)
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
	req := httptest.NewRequest(http.MethodPost, "/v1/state/reconcile", strings.NewReader(body))
	return doReconcileRequest(t, handler, req)
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
		if len(provider.observeCalls) != 0 || len(provider.applyCalls) != 0 {
			t.Fatalf("%s: provider called without state", name)
		}
	}
}

func TestReconcileObserverUnavailable(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 3, `[]`)
	// recordingProvider implements Apply only, not Observer.
	handler := HandlerWithProviderAndStateFile(&recordingProvider{}, path)

	res := doReconcile(t, handler, `{}`)
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeObserverUnavailable {
		t.Fatalf("unexpected body %+v", res)
	}
}

func TestReconcileObserverUnavailableBeforeLock(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 3, `[]`)
	handler := HandlerWithProviderAndStateFile(&recordingProvider{}, path)

	unlock, ok := tryLockState(newStateBackend(path).path)
	if !ok {
		t.Fatal("could not take state lock")
	}
	defer unlock()

	res := doReconcile(t, handler, `{}`)
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (observer check precedes lock)", res.status)
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
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(method, "/v1/state/reconcile", nil))
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
	if len(provider.observeCalls) != 0 || len(provider.applyCalls) != 0 {
		t.Fatalf("provider must not be called for invalid JSON")
	}
}

func TestReconcileEnvelopeErrorsAre422(t *testing.T) {
	provider := &reconcileProvider{}
	handler := HandlerWithProviderAndStateFile(provider, stateFilePath(t))
	cases := map[string]string{
		"not an object":     `[]`,
		"null":              `null`,
		"unknown field":     `{"expectedRevision": 0, "unexpected": true}`,
		"negative":          `{"expectedRevision": -1}`,
		"fractional":        `{"expectedRevision": 1.5}`,
		"string":            `{"expectedRevision": "0"}`,
		"null revision":     `{"expectedRevision": null}`,
		"boolean":           `{"expectedRevision": true}`,
		"object":            `{"expectedRevision": {}}`,
		"overflow exponent": `{"expectedRevision": 1e999}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			res := doReconcile(t, handler, body)
			if res.status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422", res.status)
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
	if len(provider.observeCalls) != 0 || len(provider.applyCalls) != 0 {
		t.Fatalf("provider must not be called for an invalid envelope")
	}
}

func TestReconcileStateLocked(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 1, `[{"address": "a", "type": "t", "properties": {}}]`)
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
	if len(provider.observeCalls) != 0 || len(provider.applyCalls) != 0 {
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
	if len(provider.observeCalls) != 0 || len(provider.applyCalls) != 0 {
		t.Fatalf("provider called for unreadable state")
	}
}

func TestReconcileExpectedRevisionConflict(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 4, `[{"address": "a", "type": "t", "properties": {}}]`)
	provider := &reconcileProvider{}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doReconcile(t, handler, `{"expectedRevision": 3}`)
	if res.status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeStateConflict || res.Errors[0].Path != "/expectedRevision" {
		t.Fatalf("errors = %+v, want state_conflict at /expectedRevision", res.Errors)
	}
	if res.CurrentRevision == nil || *res.CurrentRevision != 4 {
		t.Fatalf("currentRevision = %v, want 4", res.CurrentRevision)
	}
	if _, present := res.raw["revision"]; present {
		t.Fatalf("conflict response must omit revision: %v", res.raw)
	}
	if len(provider.observeCalls) != 0 || len(provider.applyCalls) != 0 {
		t.Fatalf("conflict must not Observe or Apply")
	}

	// A matching expectation proceeds.
	match := doReconcile(t, handler, `{"expectedRevision": 4}`)
	if match.status != http.StatusOK || match.Revision == nil || *match.Revision != 4 {
		t.Fatalf("matching run: status = %d revision = %v", match.status, match.Revision)
	}
}

func TestReconcileEmptyStateSkipsObserverAndApply(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 7, `[]`)
	provider := &reconcileProvider{}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doReconcile(t, handler, `{}`)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("status = %d errors = %v", res.status, res.Errors)
	}
	if res.Applied == nil || len(*res.Applied) != 0 {
		t.Fatalf("applied = %v, want empty array", res.Applied)
	}
	if res.Summary == nil || *res.Summary != (planSummary{}) {
		t.Fatalf("summary = %+v, want all zero", res.Summary)
	}
	if res.Revision == nil || *res.Revision != 7 {
		t.Fatalf("revision = %v, want 7", res.Revision)
	}
	if res.State == nil || len(res.State.Resources) != 0 {
		t.Fatalf("state = %+v, want empty resources", res.State)
	}
	if len(provider.observeCalls) != 0 || len(provider.applyCalls) != 0 {
		t.Fatalf("empty state must not reach the provider")
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
	if res.status != http.StatusOK || res.Revision == nil || *res.Revision != 0 {
		t.Fatalf("status = %d revision = %v, want 200/0", res.status, res.Revision)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("reconcile must not create the state file")
	}
}

// jsonNumberString renders a decoded JSON number, fatally failing on any
// other type.
func jsonNumberString(t *testing.T, v any) string {
	t.Helper()
	n, ok := v.(json.Number)
	if !ok {
		t.Fatalf("value %v (%T) is not a json.Number", v, v)
	}
	return n.String()
}

func TestReconcileMixedOutcomesOrderingAndSnapshots(t *testing.T) {
	path := stateFilePath(t)
	// Unordered in the file; a depends on b so topological execution runs b
	// first even though address order observes a first.
	writeDriftStateFile(t, path, 11, `[
	  {"address": "a", "type": "vm", "properties": {"size": 2}, "dependsOn": ["b"]},
	  {"address": "b", "type": "vm", "properties": {"size": 1}},
	  {"address": "c", "type": "net", "properties": {"cidr": "10.0.0.0/8"}}
	]`)
	provider := &reconcileProvider{snapshots: map[string]*Snapshot{
		// a is missing remotely -> create.
		// b exists but differs -> update; observed carries unsorted deps to
		// prove before is normalized.
		"b": {Type: "vm", Properties: map[string]any{"size": 0, "marker": true}, DependsOn: []string{"c", "a"}},
		// c is equivalent (number spelling and key order differ) -> noop.
		"c": {Type: "net", Properties: map[string]any{"cidr": "10.0.0.0/8"}},
	}}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doReconcile(t, handler, `{}`)
	if res.status != http.StatusOK || !res.Valid || len(res.Errors) != 0 {
		t.Fatalf("status = %d errors = %v raw = %v", res.status, res.Errors, res.raw)
	}
	if got, want := provider.observeCalls, []string{"a", "b", "c"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("observe order = %v, want %v", got, want)
	}
	if res.Summary == nil || *res.Summary != (planSummary{Create: 1, Update: 1, Noop: 1}) {
		t.Fatalf("summary = %+v, want {create:1 update:1 delete:0 noop:1}", res.Summary)
	}
	if res.Applied == nil || len(*res.Applied) != 2 {
		t.Fatalf("applied = %+v, want 2 changes", res.Applied)
	}
	// Topological execution: b (dependency) before a.
	applied := *res.Applied
	if applied[0].Address != "b" || applied[0].Action != "update" {
		t.Fatalf("applied[0] = %+v, want update b", applied[0])
	}
	if applied[1].Address != "a" || applied[1].Action != "create" {
		t.Fatalf("applied[1] = %+v, want create a", applied[1])
	}
	if got := applyCallSummary(provider.applyCalls); got != "update:b|create:a" {
		t.Fatalf("apply calls = %q, want update:b|create:a", got)
	}

	// update: before is the observed snapshot (normalized), after is saved.
	updateCall := provider.applyCalls[0]
	if updateCall.Before == nil || updateCall.After == nil {
		t.Fatalf("update must carry before and after: %+v", updateCall)
	}
	if jsonNumberString(t, updateCall.Before.Properties["size"]) != "0" || updateCall.Before.Properties["marker"] != true {
		t.Fatalf("before = %+v, want observed snapshot (size 0, marker true)", updateCall.Before)
	}
	if got := updateCall.Before.DependsOn; len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Fatalf("before.dependsOn = %v, want [a c]", got)
	}
	if jsonNumberString(t, updateCall.After.Properties["size"]) != "1" {
		t.Fatalf("after = %+v, want saved snapshot size 1", updateCall.After)
	}
	if _, present := updateCall.After.Properties["marker"]; present {
		t.Fatalf("after = %+v, must be the saved snapshot without marker", updateCall.After)
	}

	// create: before omitted, after is the saved snapshot.
	createCall := provider.applyCalls[1]
	if createCall.Before != nil {
		t.Fatalf("create must omit before: %+v", createCall.Before)
	}
	if createCall.After == nil || createCall.After.DependsOn[0] != "b" {
		t.Fatalf("create after = %+v, want saved snapshot depending on b", createCall.After)
	}
	if _, present := rawAppliedField(t, applied[1], "before"); present {
		t.Fatalf("raw create change must omit before")
	}

	// The response carries the original state, unchanged and address-sorted.
	if res.State == nil || len(res.State.Resources) != 3 {
		t.Fatalf("state = %+v, want original 3 resources", res.State)
	}
	if got := res.State.Resources[0].Address; got != "a" {
		t.Fatalf("state[0] = %q, want a (address sorted)", got)
	}
	if res.Revision == nil || *res.Revision != 11 {
		t.Fatalf("revision = %v, want 11", res.Revision)
	}

	// Nothing is persisted: revision and contents are untouched.
	rev, resources := readStateFileRaw(t, path)
	if rev != 11 || len(resources) != 3 {
		t.Fatalf("state file changed: rev=%d resources=%v", rev, resources)
	}
}

// applyCallSummary renders the recorded Apply requests as action:address.
func applyCallSummary(calls []ChangeRequest) string {
	parts := make([]string, 0, len(calls))
	for _, c := range calls {
		parts = append(parts, c.Action+":"+c.Address)
	}
	return strings.Join(parts, "|")
}

// rawAppliedField reports whether the raw JSON of a change carried field.
func rawAppliedField(t *testing.T, change planChange, field string) (any, bool) {
	t.Helper()
	data, err := json.Marshal(change)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	v, ok := m[field]
	return v, ok
}

func TestReconcileNoopRunDoesNotApply(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 2, `[
	  {"address": "a", "type": "t", "properties": {"n": 1}, "dependsOn": ["b"]},
	  {"address": "b", "type": "t", "properties": {}}
	]`)
	provider := &reconcileProvider{snapshots: map[string]*Snapshot{
		// Equivalent despite number spelling and dependency order.
		"a": {Type: "t", Properties: map[string]any{"n": 1.0}, DependsOn: []string{"b"}},
		"b": {Type: "t", Properties: map[string]any{}},
	}}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doReconcile(t, handler, `{}`)
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.status)
	}
	if res.Summary == nil || res.Summary.Noop != 2 || res.Summary.Update != 0 || res.Summary.Create != 0 || res.Summary.Delete != 0 {
		t.Fatalf("summary = %+v, want 2 noops", res.Summary)
	}
	if res.Applied == nil || len(*res.Applied) != 0 {
		t.Fatalf("applied = %v, want empty", res.Applied)
	}
	if len(provider.applyCalls) != 0 {
		t.Fatalf("Apply must not be called for noops: %v", applyCallSummary(provider.applyCalls))
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
	if res.Errors[0].Code != codeObserverError || res.Errors[0].Message != "remote exploded" || res.Errors[0].Path != "/resources/1" {
		t.Fatalf("unexpected error %+v", res.Errors[0])
	}
	if got, want := provider.observeCalls, []string{"a", "b"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("observe calls = %v, want %v (run must stop)", got, want)
	}
	if len(provider.applyCalls) != 0 {
		t.Fatalf("observer failure must not execute changes")
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
	}
	for name, snapshot := range cases {
		t.Run(name, func(t *testing.T) {
			path := stateFilePath(t)
			writeDriftStateFile(t, path, 2, `[
			  {"address": "a", "type": "t", "properties": {}},
			  {"address": "b", "type": "t", "properties": {}}
			]`)
			provider := &reconcileProvider{snapshots: map[string]*Snapshot{
				"a": snapshot,
				"b": {Type: "t", Properties: map[string]any{}},
			}}
			handler := HandlerWithProviderAndStateFile(provider, path)

			res := doReconcile(t, handler, `{}`)
			if res.status != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502", res.status)
			}
			if len(res.Errors) != 1 || res.Errors[0].Code != codeObserverInvalidResult || res.Errors[0].Path != "/resources/0" {
				t.Fatalf("unexpected errors %+v", res.Errors)
			}
			if len(provider.applyCalls) != 0 {
				t.Fatalf("invalid observation must not execute changes")
			}
		})
	}
}

func TestReconcileProviderFailureStopsWithoutRollbackOrWrite(t *testing.T) {
	path := stateFilePath(t)
	// Both missing remotely; topological order creates b then a; failing the
	// first leaves nothing applied, failing the second leaves b applied.
	writeDriftStateFile(t, path, 6, `[
	  {"address": "a", "type": "t", "properties": {}, "dependsOn": ["b"]},
	  {"address": "b", "type": "t", "properties": {}}
	]`)
	provider := &reconcileProvider{
		failAt:  1,
		failErr: errors.New("cannot create a"),
	}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doReconcile(t, handler, `{}`)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if len(res.Errors) != 1 {
		t.Fatalf("errors = %+v, want exactly one", res.Errors)
	}
	e := res.Errors[0]
	if e.Code != codeProviderError || e.Message != "cannot create a" || e.Path != "/changes/1" {
		t.Fatalf("error = %+v", e)
	}
	if got := applyCallSummary(provider.applyCalls); got != "create:b|create:a" {
		t.Fatalf("apply calls = %q, want both attempted (second failed)", got)
	}
	if res.Applied == nil || len(*res.Applied) != 1 || (*res.Applied)[0].Address != "b" {
		t.Fatalf("applied = %+v, want only b", res.Applied)
	}
	// The full summary survives, including the failed change.
	if res.Summary == nil || res.Summary.Create != 2 {
		t.Fatalf("summary = %+v, want create 2", res.Summary)
	}
	// State and revision remain the originals; no rollback of remote b.
	if res.Revision == nil || *res.Revision != 6 {
		t.Fatalf("revision = %v, want 6", res.Revision)
	}
	if res.State == nil || len(res.State.Resources) != 2 {
		t.Fatalf("state = %+v, want both original resources", res.State)
	}
	rev, resources := readStateFileRaw(t, path)
	if rev != 6 || len(resources) != 2 {
		t.Fatalf("state file written after provider failure: rev=%d resources=%v", rev, resources)
	}
}

func TestReconcileProviderFailureOnFirstChange(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 1, `[{"address": "a", "type": "t", "properties": {}}]`)
	provider := &reconcileProvider{failAt: 0, failErr: errors.New("boom")}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doReconcile(t, handler, `{}`)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if res.Errors[0].Path != "/changes/0" {
		t.Fatalf("path = %q, want /changes/0", res.Errors[0].Path)
	}
	if res.Applied == nil || len(*res.Applied) != 0 {
		t.Fatalf("applied = %+v, want empty", res.Applied)
	}
	if res.Revision == nil || *res.Revision != 1 {
		t.Fatalf("revision = %v, want 1", res.Revision)
	}
}

func TestReconcileLockReleasedAfterRun(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 1, `[]`)
	provider := &reconcileProvider{}
	handler := HandlerWithProviderAndStateFile(provider, path)

	if res := doReconcile(t, handler, `{}`); res.status != http.StatusOK {
		t.Fatalf("first run: status = %d", res.status)
	}
	unlock, ok := tryLockState(newStateBackend(path).path)
	if !ok {
		t.Fatal("lock was not released after the run")
	}
	defer unlock()
}

func TestReconcilePassesRequestContext(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 1, `[
	  {"address": "a", "type": "t", "properties": {}, "dependsOn": ["b"]},
	  {"address": "b", "type": "t", "properties": {}}
	]`)
	provider := &reconcileProvider{}
	handler := HandlerWithProviderAndStateFile(provider, path)

	type ctxKey struct{}
	req := httptest.NewRequest(http.MethodPost, "/v1/state/reconcile", strings.NewReader(`{}`))
	ctx := context.WithValue(req.Context(), ctxKey{}, "marker")
	req = req.WithContext(ctx)
	provider.applyCtx = ctx
	res := doReconcileRequest(t, handler, req)
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.status)
	}
	if provider.observeCtx == nil || provider.observeCtx.Value(ctxKey{}) != "marker" {
		t.Fatalf("Observe did not receive the request context unchanged")
	}
	for _, call := range provider.applyCalls {
		if call.Context == nil || call.Context.Value(ctxKey{}) != "marker" {
			t.Fatalf("Apply did not receive the request context unchanged")
		}
	}
}

func TestReconcileKeepsOtherEndpointsWorking(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 1, `[]`)
	handler := HandlerWithProviderAndStateFile(&reconcileProvider{}, path)

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("healthz status = %d", health.Code)
	}

	drift := httptest.NewRecorder()
	handler.ServeHTTP(drift, httptest.NewRequest(http.MethodGet, "/v1/state/drift", nil))
	if drift.Code != http.StatusOK {
		t.Fatalf("drift status = %d", drift.Code)
	}

	state := httptest.NewRecorder()
	handler.ServeHTTP(state, httptest.NewRequest(http.MethodGet, "/v1/state", nil))
	if state.Code != http.StatusOK {
		t.Fatalf("state GET status = %d", state.Code)
	}
}
