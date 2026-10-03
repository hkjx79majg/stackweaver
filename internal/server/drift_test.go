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

// observingProvider is a Provider that also implements Observer. Snapshots
// maps address to the remote view; a nil (or absent) entry reports the
// resource as missing. Observe calls are recorded in order.
type observingProvider struct {
	recordingProvider
	snapshots map[string]*Snapshot
	errs      map[string]error
	observed  []string
	ctx       context.Context
}

func (p *observingProvider) Observe(ctx context.Context, address string) (*Snapshot, error) {
	p.observed = append(p.observed, address)
	p.ctx = ctx
	if err, fail := p.errs[address]; fail {
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

func doDrift(t *testing.T, handler http.Handler) driftResult {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/state/drift", nil))
	return decodeDrift(t, rec)
}

func decodeDrift(t *testing.T, rec *httptest.ResponseRecorder) driftResult {
	t.Helper()
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

// seedDriftState writes a state file directly with the given revision and
// resources JSON array.
func seedDriftState(t *testing.T, path string, revision int64, resources string) {
	t.Helper()
	doc := fmt.Sprintf(`{"revision": %d, "state": {"resources": %s}}`, revision, resources)
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatalf("seed state file: %v", err)
	}
}

func TestDriftUnavailableWithoutStateFile(t *testing.T) {
	provider := &observingProvider{}
	for _, handler := range map[string]http.Handler{
		"Handler":             Handler(),
		"HandlerWithProvider": HandlerWithProvider(provider),
	} {
		res := doDrift(t, handler)
		if res.status != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", res.status)
		}
		if len(res.Errors) != 1 || res.Errors[0].Code != codeStateUnavailable {
			t.Fatalf("errors = %+v, want state_unavailable", res.Errors)
		}
	}
	if len(provider.observed) != 0 {
		t.Fatalf("observer called without state file: %v", provider.observed)
	}
}

func TestDriftRejectsOtherMethods(t *testing.T) {
	handler := HandlerWithProviderAndStateFile(&observingProvider{}, stateFilePath(t))
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(method, "/v1/state/drift", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
			t.Fatalf("%s: Allow = %q, want GET", method, allow)
		}
	}
}

func TestDriftWithoutObserverIsUnavailable(t *testing.T) {
	for name, provider := range map[string]Provider{
		"nil":       nil,
		"applyOnly": &recordingProvider{},
	} {
		handler := HandlerWithProviderAndStateFile(provider, stateFilePath(t))
		res := doDrift(t, handler)
		if res.status != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d, want 503", name, res.status)
		}
		if len(res.Errors) != 1 || res.Errors[0].Code != codeObserverUnavailable {
			t.Fatalf("%s: errors = %+v, want observer_unavailable", name, res.Errors)
		}
	}
}

func TestDriftObserverUnavailableCheckedBeforeLock(t *testing.T) {
	path := stateFilePath(t)
	unlock, ok := tryLockState(newStateBackend(path).path)
	if !ok {
		t.Fatal("could not take state lock")
	}
	defer unlock()

	handler := HandlerWithProviderAndStateFile(&recordingProvider{}, path)
	res := doDrift(t, handler)
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeObserverUnavailable {
		t.Fatalf("errors = %+v, want observer_unavailable (checked before the lock)", res.Errors)
	}
}

func TestDriftLockedStateIsConflict(t *testing.T) {
	path := stateFilePath(t)
	unlock, ok := tryLockState(newStateBackend(path).path)
	if !ok {
		t.Fatal("could not take state lock")
	}
	defer unlock()

	provider := &observingProvider{}
	handler := HandlerWithProviderAndStateFile(provider, path)
	res := doDrift(t, handler)
	if res.status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeStateLocked {
		t.Fatalf("errors = %+v, want state_locked", res.Errors)
	}
	if len(provider.observed) != 0 {
		t.Fatalf("observer called while locked: %v", provider.observed)
	}
}

func TestDriftInvalidStateFileIsReadError(t *testing.T) {
	path := stateFilePath(t)
	if err := os.WriteFile(path, []byte(`{"revision": "seven"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	provider := &observingProvider{}
	handler := HandlerWithProviderAndStateFile(provider, path)
	res := doDrift(t, handler)
	if res.status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeStateReadError {
		t.Fatalf("errors = %+v, want state_read_error", res.Errors)
	}
	if len(provider.observed) != 0 {
		t.Fatalf("observer called for unreadable state: %v", provider.observed)
	}
}

func TestDriftEmptyStateNeverObserves(t *testing.T) {
	provider := &observingProvider{}
	handler := HandlerWithProviderAndStateFile(provider, stateFilePath(t))
	res := doDrift(t, handler)
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.status)
	}
	if !res.Valid || len(res.Errors) != 0 {
		t.Fatalf("valid = %v, errors = %+v; want valid with no errors", res.Valid, res.Errors)
	}
	if res.Revision == nil || *res.Revision != 0 {
		t.Fatalf("revision = %v, want 0", res.Revision)
	}
	if res.Drifts == nil || len(*res.Drifts) != 0 {
		t.Fatalf("drifts = %+v, want empty array", res.Drifts)
	}
	if _, present := res.raw["drifts"]; !present {
		t.Fatalf("drifts key missing from response: %v", res.raw)
	}
	if res.Summary == nil || *res.Summary != (driftSummary{}) {
		t.Fatalf("summary = %+v, want all zeros", res.Summary)
	}
	if len(provider.observed) != 0 {
		t.Fatalf("observer called for empty state: %v", provider.observed)
	}
}

func TestDriftReportsChangedAndMissing(t *testing.T) {
	path := stateFilePath(t)
	seedDriftState(t, path, 3, `[
	  {"address": "b.changed", "type": "t", "properties": {"n": 1}},
	  {"address": "a.same", "type": "t", "properties": {"x": [1, 2]}, "dependsOn": ["b.changed"]},
	  {"address": "c.gone", "type": "t", "properties": {}, "dependsOn": ["a.same", "b.changed"]}
	]`)

	provider := &observingProvider{snapshots: map[string]*Snapshot{
		// Key order, number spelling, and dependency order must not matter.
		"a.same":    {Type: "t", Properties: map[string]any{"x": []any{1.0, json.Number("2")}}, DependsOn: []string{"b.changed"}},
		"b.changed": {Type: "t", Properties: map[string]any{"n": 2}},
		"c.gone":    nil,
	}}
	handler := HandlerWithProviderAndStateFile(provider, path)
	before := fileContent(t, path)
	res := doDrift(t, handler)

	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %v)", res.status, res.raw)
	}
	if !res.Valid || len(res.Errors) != 0 {
		t.Fatalf("valid = %v, errors = %+v; want valid with no errors", res.Valid, res.Errors)
	}
	if res.Revision == nil || *res.Revision != 3 {
		t.Fatalf("revision = %v, want 3", res.Revision)
	}
	if res.Summary == nil || *res.Summary != (driftSummary{Unchanged: 1, Changed: 1, Missing: 1}) {
		t.Fatalf("summary = %+v, want {1 1 1}", res.Summary)
	}
	if got, want := fmt.Sprint(provider.observed), "[a.same b.changed c.gone]"; got != want {
		t.Fatalf("observed order = %s, want %s", got, want)
	}
	if provider.ctx == nil {
		t.Fatal("observer did not receive the request context")
	}

	if res.Drifts == nil || len(*res.Drifts) != 2 {
		t.Fatalf("drifts = %+v, want 2 entries", res.Drifts)
	}
	drifts := *res.Drifts
	if drifts[0].Address != "b.changed" || drifts[0].Status != "changed" {
		t.Fatalf("drifts[0] = %+v, want b.changed changed", drifts[0])
	}
	if drifts[0].Before == nil || drifts[0].Before.Type != "t" {
		t.Fatalf("drifts[0].before = %+v, want stored snapshot", drifts[0].Before)
	}
	if drifts[0].Observed == nil || drifts[0].Observed.Properties["n"] != float64(2) {
		t.Fatalf("drifts[0].observed = %+v, want live snapshot", drifts[0].Observed)
	}
	if drifts[1].Address != "c.gone" || drifts[1].Status != "missing" {
		t.Fatalf("drifts[1] = %+v, want c.gone missing", drifts[1])
	}
	if drifts[1].Observed != nil {
		t.Fatalf("missing entry must omit observed, got %+v", drifts[1].Observed)
	}
	if got, want := fmt.Sprint(drifts[1].Before.DependsOn), "[a.same b.changed]"; got != want {
		t.Fatalf("before.dependsOn = %s, want sorted %s", got, want)
	}

	// Drift is read-only: the file and its revision stay untouched.
	if after := fileContent(t, path); after != before {
		t.Fatalf("state file changed by drift run:\nbefore %s\nafter  %s", before, after)
	}
	if len(provider.recordingProvider.calls) != 0 {
		t.Fatalf("Apply called during drift run: %+v", provider.recordingProvider.calls)
	}
}

func TestDriftAllSameReturnsEmptyDrifts(t *testing.T) {
	path := stateFilePath(t)
	seedDriftState(t, path, 7, `[
	  {"address": "r.one", "type": "t", "properties": {"a": 1}}
	]`)
	provider := &observingProvider{snapshots: map[string]*Snapshot{
		"r.one": {Type: "t", Properties: map[string]any{"a": json.Number("1.0")}},
	}}
	handler := HandlerWithProviderAndStateFile(provider, path)
	res := doDrift(t, handler)
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.status)
	}
	if res.Drifts == nil || len(*res.Drifts) != 0 {
		t.Fatalf("drifts = %+v, want empty array", res.Drifts)
	}
	if res.Summary == nil || *res.Summary != (driftSummary{Unchanged: 1}) {
		t.Fatalf("summary = %+v, want {Unchanged: 1}", res.Summary)
	}
}

func TestDriftObserverErrorStopsRun(t *testing.T) {
	path := stateFilePath(t)
	seedDriftState(t, path, 2, `[
	  {"address": "a.ok", "type": "t", "properties": {}},
	  {"address": "b.boom", "type": "t", "properties": {}},
	  {"address": "c.never", "type": "t", "properties": {}}
	]`)
	provider := &observingProvider{
		snapshots: map[string]*Snapshot{
			"a.ok":    {Type: "t", Properties: map[string]any{}},
			"c.never": {Type: "t", Properties: map[string]any{}},
		},
		errs: map[string]error{"b.boom": errors.New("cloud exploded")},
	}
	handler := HandlerWithProviderAndStateFile(provider, path)
	res := doDrift(t, handler)

	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if res.Valid {
		t.Fatal("valid = true on observer error")
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeObserverError {
		t.Fatalf("errors = %+v, want observer_error", res.Errors)
	}
	if res.Errors[0].Message != "cloud exploded" {
		t.Fatalf("message = %q, want the observer's error text", res.Errors[0].Message)
	}
	if res.Errors[0].Path != "/resources/1" {
		t.Fatalf("path = %q, want /resources/1 (sorted index)", res.Errors[0].Path)
	}
	if _, present := res.raw["drifts"]; present {
		t.Fatalf("failure response must not carry drifts: %v", res.raw)
	}
	if got, want := fmt.Sprint(provider.observed), "[a.ok b.boom]"; got != want {
		t.Fatalf("observed = %s, want run stopped after the failure: %s", got, want)
	}
	if after := readStateRevision(t, path); after != 2 {
		t.Fatalf("revision = %d, want 2 (drift must not bump it)", after)
	}
}

func TestDriftInvalidObservedSnapshots(t *testing.T) {
	cases := map[string]*Snapshot{
		"empty type":        {Type: "", Properties: map[string]any{}},
		"nil properties":    {Type: "t", Properties: nil},
		"empty dependency":  {Type: "t", Properties: map[string]any{}, DependsOn: []string{""}},
		"duplicate depends": {Type: "t", Properties: map[string]any{}, DependsOn: []string{"x", "x"}},
		"unencodable props": {Type: "t", Properties: map[string]any{"f": func() {}}},
	}
	for name, snapshot := range cases {
		t.Run(name, func(t *testing.T) {
			path := stateFilePath(t)
			seedDriftState(t, path, 1, `[
			  {"address": "r.bad", "type": "t", "properties": {}},
			  {"address": "z.never", "type": "t", "properties": {}}
			]`)
			provider := &observingProvider{snapshots: map[string]*Snapshot{
				"r.bad":   snapshot,
				"z.never": {Type: "t", Properties: map[string]any{}},
			}}
			handler := HandlerWithProviderAndStateFile(provider, path)
			res := doDrift(t, handler)

			if res.status != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502", res.status)
			}
			if len(res.Errors) != 1 || res.Errors[0].Code != codeObserverInvalidResult {
				t.Fatalf("errors = %+v, want observer_invalid_result", res.Errors)
			}
			if res.Errors[0].Path != "/resources/0" {
				t.Fatalf("path = %q, want /resources/0", res.Errors[0].Path)
			}
			if _, present := res.raw["drifts"]; present {
				t.Fatalf("failure response must not carry drifts: %v", res.raw)
			}
			if got, want := fmt.Sprint(provider.observed), "[r.bad]"; got != want {
				t.Fatalf("observed = %s, want run stopped at the invalid result: %s", got, want)
			}
		})
	}
}

func TestDriftTypeAndDependencyDifferencesAreChanges(t *testing.T) {
	path := stateFilePath(t)
	seedDriftState(t, path, 1, `[
	  {"address": "a.type", "type": "t", "properties": {}},
	  {"address": "b.deps", "type": "t", "properties": {}, "dependsOn": ["a.type"]}
	]`)
	provider := &observingProvider{snapshots: map[string]*Snapshot{
		"a.type": {Type: "other", Properties: map[string]any{}},
		"b.deps": {Type: "t", Properties: map[string]any{}, DependsOn: []string{}},
	}}
	handler := HandlerWithProviderAndStateFile(provider, path)
	res := doDrift(t, handler)
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.status)
	}
	if res.Summary == nil || *res.Summary != (driftSummary{Changed: 2}) {
		t.Fatalf("summary = %+v, want {Changed: 2}", res.Summary)
	}
	if res.Drifts == nil || len(*res.Drifts) != 2 {
		t.Fatalf("drifts = %+v, want 2 changed entries", res.Drifts)
	}
	for _, entry := range *res.Drifts {
		if entry.Status != "changed" || entry.Observed == nil {
			t.Fatalf("entry = %+v, want changed with observed", entry)
		}
	}
}

func TestDriftLockReleasedAfterRun(t *testing.T) {
	path := stateFilePath(t)
	provider := &observingProvider{}
	handler := HandlerWithProviderAndStateFile(provider, path)
	if res := doDrift(t, handler); res.status != http.StatusOK {
		t.Fatalf("first run: status = %d, want 200", res.status)
	}
	unlock, ok := tryLockState(newStateBackend(path).path)
	if !ok {
		t.Fatal("state lock still held after the drift run")
	}
	unlock()
}

func readStateRevision(t *testing.T, path string) int64 {
	t.Helper()
	revision, _ := readStateFileRaw(t, path)
	return revision
}
