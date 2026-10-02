package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// stateConfigBody builds a state-apply request envelope around the given
// configuration resources, mirroring planBody's configuration half.
func stateConfigBody(configResources string) string {
	return `{
	  "configuration": {
	    "resourceTypes": [{"name": "t", "schema": {"type": "object", "additionalProperties": true}}],
	    "resources": ` + configResources + `
	  }
	}`
}

type stateApplyResult struct {
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

func doStateApply(t *testing.T, handler http.Handler, body string) stateApplyResult {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/state/apply", strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var res stateApplyResult
	res.status = rec.Code
	if err := json.Unmarshal(rec.Body.Bytes(), &res.raw); err != nil {
		t.Fatalf("response is not JSON: %v (body %q)", err, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode typed response: %v (body %q)", err, rec.Body.String())
	}
	return res
}

type stateGetResult struct {
	status   int
	Revision int64 `json:"revision"`
	State    *struct {
		Resources []stateResource `json:"resources"`
	} `json:"state"`
}

func doStateGet(t *testing.T, handler http.Handler) stateGetResult {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/state", nil))
	var res stateGetResult
	res.status = rec.Code
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("decode GET response: %v (body %q)", err, rec.Body.String())
		}
	}
	return res
}

func stateFilePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "state.json")
}

func readStateFileRaw(t *testing.T, path string) (revision int64, resources []stateResource) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	var doc struct {
		Revision int64 `json:"revision"`
		State    struct {
			Resources []stateResource `json:"resources"`
		} `json:"state"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("state file is not valid JSON: %v (content %q)", err, data)
	}
	return doc.Revision, doc.State.Resources
}

func fileContent(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	return string(data)
}

func TestStateEndpointsUnavailableWithoutStateFile(t *testing.T) {
	for _, handler := range map[string]http.Handler{
		"Handler":             Handler(),
		"HandlerWithProvider": HandlerWithProvider(&recordingProvider{}),
	} {
		for _, target := range []struct{ method, path string }{
			{http.MethodGet, "/v1/state"},
			{http.MethodPost, "/v1/state/apply"},
		} {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(target.method, target.path, nil))
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("%s %s: status = %d, want 503", target.method, target.path, rec.Code)
			}
			var payload validateResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatalf("%s %s: body is not JSON: %v", target.method, target.path, err)
			}
			if len(payload.Errors) != 1 || payload.Errors[0].Code != codeStateUnavailable {
				t.Fatalf("%s %s: errors = %+v, want state_unavailable", target.method, target.path, payload.Errors)
			}
		}
	}
}

func TestStateGetMissingFileIsRevisionZero(t *testing.T) {
	handler := HandlerWithProviderAndStateFile(nil, stateFilePath(t))
	res := doStateGet(t, handler)
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.status)
	}
	if res.Revision != 0 {
		t.Fatalf("revision = %d, want 0", res.Revision)
	}
	if res.State == nil || len(res.State.Resources) != 0 {
		t.Fatalf("state = %+v, want empty resources", res.State)
	}
}

func TestStateGetRejectsOtherMethods(t *testing.T) {
	handler := HandlerWithProviderAndStateFile(nil, stateFilePath(t))
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(method, "/v1/state", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
			t.Fatalf("%s: Allow = %q, want GET", method, allow)
		}
	}
}

func TestStateApplyRejectsOtherMethods(t *testing.T) {
	handler := HandlerWithProviderAndStateFile(nil, stateFilePath(t))
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(method, "/v1/state/apply", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want 405", method, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
			t.Fatalf("%s: Allow = %q, want POST", method, allow)
		}
	}
}

func TestStateGetUnreadableFileIsReadError(t *testing.T) {
	path := stateFilePath(t)
	for _, bad := range []string{
		`not json`,
		`{"revision": -1, "state": {"resources": []}}`,
		`{"revision": 1.5, "state": {"resources": []}}`,
		`{"revision": "2", "state": {"resources": []}}`,
		`{"state": {"resources": []}}`,
		`{"revision": 2}`,
		`{"revision": 2, "state": {"resources": [{}]}}`,
		`[]`,
	} {
		if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
			t.Fatal(err)
		}
		handler := HandlerWithProviderAndStateFile(nil, path)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/state", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("content %q: status = %d, want 500", bad, rec.Code)
		}
		var payload validateResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("content %q: body is not JSON: %v", bad, err)
		}
		if len(payload.Errors) != 1 || payload.Errors[0].Code != codeStateReadError {
			t.Fatalf("content %q: errors = %+v, want state_read_error", bad, payload.Errors)
		}
		if got := fileContent(t, path); got != bad {
			t.Fatalf("content %q: file was rewritten to %q", bad, got)
		}
	}
}

func TestStateApplyInvalidJSONIs400(t *testing.T) {
	handler := HandlerWithProviderAndStateFile(&recordingProvider{}, stateFilePath(t))
	for _, body := range []string{"", `{`, stateConfigBody(`[]`) + ` {}`} {
		res := doStateApply(t, handler, body)
		if res.status != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", res.status)
		}
		if res.Valid || len(res.Errors) != 1 || res.Errors[0].Code != codeInvalidJSON {
			t.Fatalf("payload = %+v", res)
		}
	}
}

func TestStateApplyEnvelopeErrorsAre422(t *testing.T) {
	provider := &recordingProvider{}
	handler := HandlerWithProviderAndStateFile(provider, stateFilePath(t))
	cases := map[string]string{
		"not an object":       `[]`,
		"empty envelope":      `{}`,
		"unknown field":       `{"configuration": {"resourceTypes": [], "resources": []}, "priorState": {"resources": []}}`,
		"negative revision":   `{"configuration": {"resourceTypes": [], "resources": []}, "expectedRevision": -1}`,
		"fractional revision": `{"configuration": {"resourceTypes": [], "resources": []}, "expectedRevision": 1.5}`,
		"string revision":     `{"configuration": {"resourceTypes": [], "resources": []}, "expectedRevision": "0"}`,
		"null revision":       `{"configuration": {"resourceTypes": [], "resources": []}, "expectedRevision": null}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			res := doStateApply(t, handler, body)
			if res.status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422", res.status)
			}
			if res.Valid || len(res.Errors) == 0 {
				t.Fatalf("payload = %+v", res)
			}
			for _, e := range res.Errors {
				if e.Code != codeInvalidStateRequest {
					t.Fatalf("error code = %q, want invalid_state_request", e.Code)
				}
			}
		})
	}
	if len(provider.calls) != 0 {
		t.Fatalf("provider must not be called, got %v", provider.calls)
	}
}

func TestStateApplyConfigurationErrorsArePrefixed(t *testing.T) {
	handler := HandlerWithProviderAndStateFile(&recordingProvider{}, stateFilePath(t))
	body := `{"configuration": {"resourceTypes": [], "resources": [{"address": "a", "type": "x", "properties": {}}]}}`
	res := doStateApply(t, handler, body)
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeUnknownResourceType {
		t.Fatalf("errors = %+v", res.Errors)
	}
	if res.Errors[0].Path != "/configuration/resources/0/type" {
		t.Fatalf("path = %q, want /configuration prefix", res.Errors[0].Path)
	}
}

func TestStateApplyEmptyPlanKeepsRevisionAndDoesNotWrite(t *testing.T) {
	path := stateFilePath(t)
	handler := HandlerWithProviderAndStateFile(nil, path)
	res := doStateApply(t, handler, stateConfigBody(`[]`))
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("status = %d valid = %v errors = %v", res.status, res.Valid, res.Errors)
	}
	if res.Revision == nil || *res.Revision != 0 {
		t.Fatalf("revision = %v, want 0", res.Revision)
	}
	if res.Applied == nil || len(*res.Applied) != 0 {
		t.Fatalf("applied = %v, want empty", res.Applied)
	}
	if res.State == nil || len(res.State.Resources) != 0 {
		t.Fatalf("state = %+v, want empty resources", res.State)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("empty plan must not create the state file")
	}
}

func TestStateApplyChangesWithoutProviderIsUnavailable(t *testing.T) {
	path := stateFilePath(t)
	handler := HandlerWithProviderAndStateFile(nil, path)
	res := doStateApply(t, handler, stateConfigBody(`[`+resEntry("a", `[]`)+`]`))
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.status)
	}
	if res.Valid || len(res.Errors) != 1 || res.Errors[0].Code != codeProviderUnavailable {
		t.Fatalf("payload = %+v", res)
	}
	if res.Summary == nil || res.Summary.Create != 1 {
		t.Fatalf("summary = %+v, want the plan summary", res.Summary)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("unavailable provider must not create the state file")
	}
}

func TestStateApplySuccessCommitsAndBumpsRevision(t *testing.T) {
	path := stateFilePath(t)
	provider := &recordingProvider{}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doStateApply(t, handler, stateConfigBody(`[`+resEntry("a", `[]`)+`]`))
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("status = %d errors = %v", res.status, res.Errors)
	}
	if res.Revision == nil || *res.Revision != 1 {
		t.Fatalf("revision = %v, want 1", res.Revision)
	}
	if res.Applied == nil || len(*res.Applied) != 1 || (*res.Applied)[0].Address != "a" {
		t.Fatalf("applied = %v, want [a]", res.Applied)
	}

	rev, resources := readStateFileRaw(t, path)
	if rev != 1 {
		t.Fatalf("file revision = %d, want 1", rev)
	}
	if len(resources) != 1 || resources[0].Address != "a" || resources[0].Type != "t" {
		t.Fatalf("file resources = %+v", resources)
	}

	// The committed file is the input of the next run: a is now a noop, so
	// the same configuration succeeds without calling the Provider and
	// keeps the revision.
	before := len(provider.calls)
	second := doStateApply(t, handler, stateConfigBody(`[`+resEntry("a", `[]`)+`]`))
	if second.status != http.StatusOK || second.Revision == nil || *second.Revision != 1 {
		t.Fatalf("second run: status = %d revision = %v", second.status, second.Revision)
	}
	if len(provider.calls) != before {
		t.Fatalf("noop run must not call the provider")
	}

	got := doStateGet(t, handler)
	if got.status != http.StatusOK || got.Revision != 1 {
		t.Fatalf("GET: status = %d revision = %d, want 200/1", got.status, got.Revision)
	}
	if got.State == nil || len(got.State.Resources) != 1 || got.State.Resources[0].Address != "a" {
		t.Fatalf("GET state = %+v", got.State)
	}
}

func TestStateApplyExpectedRevisionConflict(t *testing.T) {
	path := stateFilePath(t)
	provider := &recordingProvider{}
	handler := HandlerWithProviderAndStateFile(provider, path)

	// Missing file: current revision is 0.
	body := strings.TrimSuffix(stateConfigBody(`[`+resEntry("a", `[]`)+`]`), "}") + `, "expectedRevision": 3}`
	res := doStateApply(t, handler, body)
	if res.status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeStateConflict {
		t.Fatalf("errors = %+v, want state_conflict", res.Errors)
	}
	if res.CurrentRevision == nil || *res.CurrentRevision != 0 {
		t.Fatalf("currentRevision = %v, want 0", res.CurrentRevision)
	}
	if len(provider.calls) != 0 {
		t.Fatalf("conflict must not call the provider")
	}

	// Matching expectation lets the run proceed.
	match := strings.TrimSuffix(stateConfigBody(`[`+resEntry("a", `[]`)+`]`), "}") + `, "expectedRevision": 0}`
	ok := doStateApply(t, handler, match)
	if ok.status != http.StatusOK || ok.Revision == nil || *ok.Revision != 1 {
		t.Fatalf("matching run: status = %d revision = %v", ok.status, ok.Revision)
	}

	// The file moved on; the stale expectation now conflicts with revision 1.
	stale := doStateApply(t, handler, match)
	if stale.status != http.StatusConflict || stale.CurrentRevision == nil || *stale.CurrentRevision != 1 {
		t.Fatalf("stale run: status = %d currentRevision = %v", stale.status, stale.CurrentRevision)
	}
}

func TestStateApplyProviderFailureOnFirstChangeKeepsFile(t *testing.T) {
	path := stateFilePath(t)
	provider := &recordingProvider{failAt: 0, failErr: errors.New("boom")}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doStateApply(t, handler, stateConfigBody(`[`+resEntry("a", `[]`)+`]`))
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeProviderError || res.Errors[0].Path != "/changes/0" {
		t.Fatalf("errors = %+v", res.Errors)
	}
	if res.Revision == nil || *res.Revision != 0 {
		t.Fatalf("revision = %v, want 0 (file untouched)", res.Revision)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("first-change failure must not create the state file")
	}
}

func TestStateApplyProviderFailureAfterSuccessCommitsPartial(t *testing.T) {
	path := stateFilePath(t)
	// Creates run in deterministic order b then a; failing the second leaves
	// b committed and bumps the revision exactly once.
	provider := &recordingProvider{failAt: 1, failErr: errors.New("boom at a")}
	handler := HandlerWithProviderAndStateFile(provider, path)
	body := stateConfigBody(`[` + resEntry("a", `["b"]`) + `,` + resEntry("b", `[]`) + `]`)

	res := doStateApply(t, handler, body)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if res.Applied == nil || len(*res.Applied) != 1 || (*res.Applied)[0].Address != "b" {
		t.Fatalf("applied = %v, want only b", res.Applied)
	}
	if res.Revision == nil || *res.Revision != 1 {
		t.Fatalf("revision = %v, want 1 (partial state committed)", res.Revision)
	}

	rev, resources := readStateFileRaw(t, path)
	if rev != 1 {
		t.Fatalf("file revision = %d, want 1", rev)
	}
	if len(resources) != 1 || resources[0].Address != "b" {
		t.Fatalf("file resources = %+v, want only b", resources)
	}
}

// blockingProvider blocks inside Apply until released, so tests can observe
// the state lock while a run is in flight.
type blockingProvider struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *blockingProvider) Apply(req ChangeRequest) error {
	p.once.Do(func() { close(p.entered) })
	<-p.release
	return nil
}

func TestStateApplyLockExcludesConcurrentRuns(t *testing.T) {
	path := stateFilePath(t)
	provider := &blockingProvider{entered: make(chan struct{}), release: make(chan struct{})}
	first := HandlerWithProviderAndStateFile(provider, path)
	// A separately created handler for the same path shares the lock.
	second := HandlerWithProviderAndStateFile(&recordingProvider{}, path)

	body := stateConfigBody(`[` + resEntry("a", `[]`) + `]`)
	type outcome struct{ status int }
	done := make(chan outcome, 1)
	go func() {
		res := doStateApply(t, first, body)
		done <- outcome{res.status}
	}()

	<-provider.entered
	res := doStateApply(t, second, body)
	if res.status != http.StatusConflict {
		t.Fatalf("concurrent run: status = %d, want 409", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeStateLocked {
		t.Fatalf("concurrent run: errors = %+v, want state_locked", res.Errors)
	}

	close(provider.release)
	if got := <-done; got.status != http.StatusOK {
		t.Fatalf("first run: status = %d, want 200", got.status)
	}

	// The lock was released: a later run proceeds.
	third := doStateApply(t, second, body)
	if third.status != http.StatusOK {
		t.Fatalf("post-lock run: status = %d, want 200", third.status)
	}
}

func TestStateApplyLockReleasedOnFailure(t *testing.T) {
	path := stateFilePath(t)
	handler := HandlerWithProviderAndStateFile(&recordingProvider{failAt: 0, failErr: errors.New("boom")}, path)
	body := stateConfigBody(`[` + resEntry("a", `[]`) + `]`)
	if res := doStateApply(t, handler, body); res.status != http.StatusBadGateway {
		t.Fatalf("failing run: status = %d, want 502", res.status)
	}
	// A failure must not hold the lock: the next run reaches the Provider.
	res := doStateApply(t, HandlerWithProviderAndStateFile(&recordingProvider{}, path), body)
	if res.status != http.StatusOK {
		t.Fatalf("follow-up run: status = %d, want 200", res.status)
	}
}

func TestStateApplyWriteFailureKeepsOriginalAndReportsPendingState(t *testing.T) {
	// A state path inside a missing directory reads as revision 0 but cannot
	// be committed, so the run fails at the write step.
	path := filepath.Join(t.TempDir(), "missing", "state.json")
	provider := &recordingProvider{}
	handler := HandlerWithProviderAndStateFile(provider, path)

	res := doStateApply(t, handler, stateConfigBody(`[`+resEntry("a", `[]`)+`]`))
	if res.status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeStateWriteError {
		t.Fatalf("errors = %+v, want state_write_error", res.Errors)
	}
	// The executed change and the state that could not be committed are
	// still reported.
	if res.Applied == nil || len(*res.Applied) != 1 || (*res.Applied)[0].Address != "a" {
		t.Fatalf("applied = %v, want [a]", res.Applied)
	}
	if res.State == nil || len(res.State.Resources) != 1 || res.State.Resources[0].Address != "a" {
		t.Fatalf("state = %+v, want pending state with a", res.State)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("failed commit must not leave a file behind")
	}
}

func TestStateApplyWriteFailurePreservesExistingFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-based test does not apply to root")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	handler := HandlerWithProviderAndStateFile(&recordingProvider{}, path)

	// Commit one resource, then make the directory read-only so the atomic
	// replace of the next commit fails.
	first := doStateApply(t, handler, stateConfigBody(`[`+resEntry("a", `[]`)+`]`))
	if first.status != http.StatusOK {
		t.Fatalf("setup run: status = %d", first.status)
	}
	original := fileContent(t, path)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)

	res := doStateApply(t, handler, stateConfigBody(`[`+resEntry("a", `[]`)+`,`+resEntry("b", `[]`)+`]`))
	if res.status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeStateWriteError {
		t.Fatalf("errors = %+v, want state_write_error", res.Errors)
	}
	if got := fileContent(t, path); got != original {
		t.Fatalf("original file changed after failed commit:\n%s", got)
	}
	// No half-written temporary file is left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		t.Fatalf("directory holds %v, want only state.json", entries)
	}
}

func TestStateApplySeparatelyCreatedHandlersShareFile(t *testing.T) {
	path := stateFilePath(t)
	first := HandlerWithProviderAndStateFile(&recordingProvider{}, path)
	second := HandlerWithProviderAndStateFile(&recordingProvider{}, path)

	if res := doStateApply(t, first, stateConfigBody(`[`+resEntry("a", `[]`)+`]`)); res.status != http.StatusOK {
		t.Fatalf("first handler: status = %d", res.status)
	}
	// The second handler observes the committed revision.
	res := doStateApply(t, second, stateConfigBody(`[`+resEntry("a", `[]`)+`,`+resEntry("b", `[]`)+`]`))
	if res.status != http.StatusOK || res.Revision == nil || *res.Revision != 2 {
		t.Fatalf("second handler: status = %d revision = %v, want 200/2", res.status, res.Revision)
	}
	if res.Applied == nil || len(*res.Applied) != 1 || (*res.Applied)[0].Address != "b" {
		t.Fatalf("second handler applied = %v, want only create b", res.Applied)
	}
}

func TestStateApplyKeepsOtherEndpointsWorking(t *testing.T) {
	handler := HandlerWithProviderAndStateFile(&recordingProvider{}, stateFilePath(t))

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("healthz status = %d", health.Code)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/plans",
		strings.NewReader(planBody(`[`+resEntry("a", `[]`)+`]`, `[]`))))
	if rec.Code != http.StatusOK {
		t.Fatalf("plans status = %d", rec.Code)
	}

	applyRec := httptest.NewRecorder()
	handler.ServeHTTP(applyRec, httptest.NewRequest(http.MethodPost, "/v1/apply",
		strings.NewReader(planBody(`[`+resEntry("a", `[]`)+`]`, `[]`))))
	if applyRec.Code != http.StatusOK {
		t.Fatalf("apply status = %d", applyRec.Code)
	}
}
