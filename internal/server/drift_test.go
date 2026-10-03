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

// driftProvider is a Provider that also implements Observer, driven by
// scripted snapshots and errors keyed by address.
type driftProvider struct {
	snapshots map[string]*Snapshot
	errs      map[string]error
	calls     []string
	applies   int
	lastCtx   context.Context
}

func (p *driftProvider) Apply(req ChangeRequest) error {
	p.applies++
	return nil
}

func (p *driftProvider) Observe(ctx context.Context, address string) (*Snapshot, error) {
	p.calls = append(p.calls, address)
	p.lastCtx = ctx
	if err := p.errs[address]; err != nil {
		return nil, err
	}
	return p.snapshots[address], nil
}

type driftResult struct {
	status   int
	raw      map[string]any
	Valid    bool              `json:"valid"`
	Errors   []validationError `json:"errors"`
	Revision *int64            `json:"revision"`
	Drifts   *[]driftEntry     `json:"drifts"`
	Summary  *driftSummary     `json:"summary"`
}

func doDriftGet(t *testing.T, handler http.Handler) driftResult {
	t.Helper()
	return doDriftRequest(t, handler, httptest.NewRequest(http.MethodGet, "/v1/state/drift", nil))
}

func doDriftRequest(t *testing.T, handler http.Handler, req *http.Request) driftResult {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var res driftResult
	res.status = rec.Code
	if err := json.Unmarshal(rec.Body.Bytes(), &res.raw); err != nil {
		t.Fatalf("response is not JSON: %v (body %q)", err, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode typed response: %v (body %q)", err, rec.Body.String())
	}
	return res
}

// writeDriftStateFile stores a state document with the given revision and
// raw resources array.
func writeDriftStateFile(t *testing.T, path string, revision int64, resources string) {
	t.Helper()
	doc := fmt.Sprintf(`{"revision": %d, "state": {"resources": %s}}`, revision, resources)
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatalf("write state file: %v", err)
	}
}

func TestDriftUnavailableWithoutStateFile(t *testing.T) {
	provider := &driftProvider{}
	for name, handler := range map[string]http.Handler{
		"Handler":             Handler(),
		"HandlerWithProvider": HandlerWithProvider(provider),
	} {
		res := doDriftGet(t, handler)
		if res.status != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d, want 503", name, res.status)
		}
		if res.Valid || len(res.Errors) != 1 || res.Errors[0].Code != codeStateUnavailable {
			t.Fatalf("%s: unexpected body %+v", name, res)
		}
		if len(provider.calls) != 0 {
			t.Fatalf("%s: Observer called %d times for unavailable state", name, len(provider.calls))
		}
	}
}

func TestDriftObserverUnavailable(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 3, `[]`)
	handler := HandlerWithProviderAndStateFile(&recordingProvider{}, path)

	res := doDriftGet(t, handler)
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.status)
	}
	if res.Valid || len(res.Errors) != 1 || res.Errors[0].Code != codeObserverUnavailable {
		t.Fatalf("unexpected body %+v", res)
	}
}

func TestDriftObserverUnavailableBeforeLock(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 3, `[]`)
	handler := HandlerWithProviderAndStateFile(&recordingProvider{}, path)

	// Even with the lock held, a missing Observer is reported first.
	unlock, ok := tryLockState(newStateBackend(path).path)
	if !ok {
		t.Fatal("could not take state lock")
	}
	defer unlock()

	res := doDriftGet(t, handler)
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeObserverUnavailable {
		t.Fatalf("unexpected body %+v", res)
	}
}

func TestDriftMethodNotAllowed(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 0, `[]`)
	handler := HandlerWithProviderAndStateFile(&driftProvider{}, path)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/v1/state/drift", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
			t.Fatalf("%s: Allow = %q, want GET", method, allow)
		}
	}
}

func TestDriftStateLocked(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 1, `[]`)
	provider := &driftProvider{}
	handler := HandlerWithProviderAndStateFile(provider, path)

	unlock, ok := tryLockState(newStateBackend(path).path)
	if !ok {
		t.Fatal("could not take state lock")
	}
	defer unlock()

	res := doDriftGet(t, handler)
	if res.status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeStateLocked {
		t.Fatalf("unexpected body %+v", res)
	}
	if len(provider.calls) != 0 {
		t.Fatalf("Observer called while locked: %v", provider.calls)
	}
}

func TestDriftStateReadError(t *testing.T) {
	path := stateFilePath(t)
	if err := os.WriteFile(path, []byte(`{"revision": "nope"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	provider := &driftProvider{}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doDriftGet(t, handler)
	if res.status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeStateReadError {
		t.Fatalf("unexpected body %+v", res)
	}
	if len(provider.calls) != 0 {
		t.Fatalf("Observer called for unreadable state: %v", provider.calls)
	}
}

func TestDriftEmptyState(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 7, `[]`)
	provider := &driftProvider{}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doDriftGet(t, handler)
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %v)", res.status, res.raw)
	}
	if !res.Valid || len(res.Errors) != 0 {
		t.Fatalf("unexpected valid/errors: %+v", res)
	}
	if res.Revision == nil || *res.Revision != 7 {
		t.Fatalf("revision = %v, want 7", res.Revision)
	}
	if res.Drifts == nil || len(*res.Drifts) != 0 {
		t.Fatalf("drifts = %v, want empty array", res.Drifts)
	}
	if res.Summary == nil || *res.Summary != (driftSummary{}) {
		t.Fatalf("summary = %+v, want all zero", res.Summary)
	}
	if len(provider.calls) != 0 {
		t.Fatalf("Observer called for empty state: %v", provider.calls)
	}
}

func TestDriftMissingFileReadsAsEmpty(t *testing.T) {
	path := stateFilePath(t)
	provider := &driftProvider{}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doDriftGet(t, handler)
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.status)
	}
	if res.Revision == nil || *res.Revision != 0 {
		t.Fatalf("revision = %v, want 0", res.Revision)
	}
	if len(provider.calls) != 0 {
		t.Fatalf("Observer called for empty state: %v", provider.calls)
	}
}

func TestDriftMixedOutcomes(t *testing.T) {
	path := stateFilePath(t)
	// Deliberately unordered in the file; output must follow address order.
	writeDriftStateFile(t, path, 11, `[
	  {"address": "res.c", "type": "vm", "properties": {"size": 2}, "dependsOn": ["res.a"]},
	  {"address": "res.a", "type": "net", "properties": {"cidr": "10.0.0.0/8", "n": 1}},
	  {"address": "res.b", "type": "vm", "properties": {"size": 1}, "dependsOn": ["res.a"]}
	]`)
	provider := &driftProvider{snapshots: map[string]*Snapshot{
		// Unchanged: key order, numeric spelling, and dependency order differ.
		"res.a": {Type: "net", Properties: map[string]any{"n": 1.0, "cidr": "10.0.0.0/8"}},
		// Changed: property value differs; dependsOn output must be sorted.
		"res.b": {Type: "vm", Properties: map[string]any{"size": 3}, DependsOn: []string{"res.a"}},
		// Missing: no snapshot.
	}}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doDriftGet(t, handler)
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %v)", res.status, res.raw)
	}
	if !res.Valid || len(res.Errors) != 0 {
		t.Fatalf("unexpected valid/errors: %+v", res)
	}
	if res.Revision == nil || *res.Revision != 11 {
		t.Fatalf("revision = %v, want 11", res.Revision)
	}
	if got, want := provider.calls, []string{"res.a", "res.b", "res.c"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("observe order = %v, want %v", got, want)
	}
	if res.Summary == nil || *res.Summary != (driftSummary{Unchanged: 1, Changed: 1, Missing: 1}) {
		t.Fatalf("summary = %+v, want {1 1 1}", res.Summary)
	}
	if res.Drifts == nil || len(*res.Drifts) != 2 {
		t.Fatalf("drifts = %+v, want 2 entries", res.Drifts)
	}
	drifts := *res.Drifts
	if drifts[0].Address != "res.b" || drifts[0].Status != "changed" {
		t.Fatalf("drifts[0] = %+v, want changed res.b", drifts[0])
	}
	if drifts[0].Before == nil || drifts[0].Observed == nil {
		t.Fatalf("changed entry must carry before and observed: %+v", drifts[0])
	}
	if got := drifts[0].Before.DependsOn; len(got) != 1 || got[0] != "res.a" {
		t.Fatalf("before.dependsOn = %v, want [res.a]", got)
	}
	if drifts[1].Address != "res.c" || drifts[1].Status != "missing" {
		t.Fatalf("drifts[1] = %+v, want missing res.c", drifts[1])
	}
	if drifts[1].Before == nil {
		t.Fatalf("missing entry must carry before: %+v", drifts[1])
	}
	if drifts[1].Observed != nil {
		t.Fatalf("missing entry must omit observed: %+v", drifts[1])
	}
	if _, present := drifts[1].rawObserved(); present {
		t.Fatalf("raw response must omit observed for missing entries: %v", res.raw)
	}
	if provider.applies != 0 {
		t.Fatalf("Apply called %d times during drift detection", provider.applies)
	}
	// The state file and its revision are untouched.
	if got := fileContent(t, path); !strings.Contains(got, `"revision": 11`) {
		t.Fatalf("state file changed: %s", got)
	}
}

// rawObserved reports whether the raw JSON of a missing entry carried an
// observed field; it is a helper over the typed decode for omitempty checks.
func (e driftEntry) rawObserved() (any, bool) {
	data, err := json.Marshal(e)
	if err != nil {
		return nil, false
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, false
	}
	v, ok := m["observed"]
	return v, ok
}

func TestDriftComparisonSemantics(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 1, `[
	  {"address": "same.num", "type": "t", "properties": {"n": 1}},
	  {"address": "same.deps", "type": "t", "properties": {}, "dependsOn": ["same.num"]},
	  {"address": "diff.type", "type": "a", "properties": {}},
	  {"address": "diff.props", "type": "t", "properties": {"list": [1, 2]}},
	  {"address": "diff.deps", "type": "t", "properties": {}, "dependsOn": ["same.num"]}
	]`)
	provider := &driftProvider{snapshots: map[string]*Snapshot{
		"same.num":  {Type: "t", Properties: map[string]any{"n": 1.0}},
		"same.deps": {Type: "t", Properties: map[string]any{}, DependsOn: []string{"same.num"}},
		"diff.type": {Type: "b", Properties: map[string]any{}},
		// Arrays are ordered: [2 1] differs from [1 2].
		"diff.props": {Type: "t", Properties: map[string]any{"list": []any{2, 1}}},
		"diff.deps":  {Type: "t", Properties: map[string]any{}},
	}}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doDriftGet(t, handler)
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %v)", res.status, res.raw)
	}
	if res.Summary == nil || *res.Summary != (driftSummary{Unchanged: 2, Changed: 3, Missing: 0}) {
		t.Fatalf("summary = %+v, want {2 3 0}", res.Summary)
	}
	if res.Drifts == nil || len(*res.Drifts) != 3 {
		t.Fatalf("drifts = %+v, want 3 entries", res.Drifts)
	}
	var got []string
	for _, d := range *res.Drifts {
		got = append(got, d.Address+":"+d.Status)
	}
	want := []string{"diff.deps:changed", "diff.props:changed", "diff.type:changed"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("drifts = %v, want %v", got, want)
	}
}

func TestDriftObserverErrorStopsRun(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 5, `[
	  {"address": "a", "type": "t", "properties": {}},
	  {"address": "b", "type": "t", "properties": {}},
	  {"address": "c", "type": "t", "properties": {}}
	]`)
	provider := &driftProvider{
		snapshots: map[string]*Snapshot{
			"a": {Type: "t", Properties: map[string]any{}},
		},
		errs: map[string]error{"b": errors.New("remote exploded")},
	}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doDriftGet(t, handler)
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
	if res.Drifts != nil {
		t.Fatalf("failure response must not carry partial drifts: %+v", res.Drifts)
	}
	if got, want := provider.calls, []string{"a", "b"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("observe calls = %v, want %v (run must stop)", got, want)
	}
	if got := fileContent(t, path); !strings.Contains(got, `"revision": 5`) {
		t.Fatalf("state file changed: %s", got)
	}
}

func TestDriftObserverInvalidResult(t *testing.T) {
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
			provider := &driftProvider{snapshots: map[string]*Snapshot{
				"a": snapshot,
				"b": {Type: "t", Properties: map[string]any{}},
			}}
			handler := HandlerWithProviderAndStateFile(provider, path)

			res := doDriftGet(t, handler)
			if res.status != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502", res.status)
			}
			if len(res.Errors) != 1 || res.Errors[0].Code != codeObserverInvalidResult {
				t.Fatalf("unexpected body %+v", res)
			}
			if res.Errors[0].Path != "/resources/0" {
				t.Fatalf("path = %q, want /resources/0", res.Errors[0].Path)
			}
			if res.Drifts != nil {
				t.Fatalf("failure response must not carry partial drifts: %+v", res.Drifts)
			}
			if got, want := provider.calls, []string{"a"}; fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("observe calls = %v, want %v (run must stop)", got, want)
			}
		})
	}
}

func TestDriftPassesRequestContext(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 1, `[{"address": "a", "type": "t", "properties": {}}]`)
	provider := &driftProvider{snapshots: map[string]*Snapshot{
		"a": {Type: "t", Properties: map[string]any{}},
	}}
	handler := HandlerWithProviderAndStateFile(provider, path)

	type ctxKey struct{}
	req := httptest.NewRequest(http.MethodGet, "/v1/state/drift", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, "marker"))
	res := doDriftRequest(t, handler, req)
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.status)
	}
	if provider.lastCtx == nil || provider.lastCtx.Value(ctxKey{}) != "marker" {
		t.Fatalf("Observer did not receive the request context unchanged")
	}
}
