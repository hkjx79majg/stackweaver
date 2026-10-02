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

// stateApplyBody builds a POST /v1/state/apply envelope around configuration
// resources of the permissive type t.
func stateApplyBody(configResources string) string {
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

func decodeStateApply(t *testing.T, rec *httptest.ResponseRecorder) stateApplyResult {
	t.Helper()
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

func doStateApply(t *testing.T, handler http.Handler, body string) stateApplyResult {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/state/apply", strings.NewReader(body)))
	return decodeStateApply(t, rec)
}

type stateGetResult struct {
	status   int
	raw      map[string]any
	Valid    bool              `json:"valid"`
	Errors   []validationError `json:"errors"`
	Revision *int64            `json:"revision"`
	State    *struct {
		Resources []stateResource `json:"resources"`
	} `json:"state"`
}

func doGetState(t *testing.T, handler http.Handler) stateGetResult {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/state", nil))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var res stateGetResult
	res.status = rec.Code
	if err := json.Unmarshal(rec.Body.Bytes(), &res.raw); err != nil {
		t.Fatalf("response is not JSON: %v (body %q)", err, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode typed response: %v (body %q)", err, rec.Body.String())
	}
	return res
}

// stateFileContents reads the persisted state file for assertions.
func stateFileContents(t *testing.T, path string) (revision int64, resources []stateResource) {
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

func fileAddresses(resources []stateResource) string {
	parts := make([]string, 0, len(resources))
	for _, r := range resources {
		parts = append(parts, r.Address)
	}
	return strings.Join(parts, "|")
}

func TestStateEndpointsUnavailableWithoutStateFile(t *testing.T) {
	for name, handler := range map[string]http.Handler{
		"Handler":             Handler(),
		"HandlerWithProvider": HandlerWithProvider(&recordingProvider{}),
	} {
		t.Run(name, func(t *testing.T) {
			for _, req := range []*http.Request{
				httptest.NewRequest(http.MethodGet, "/v1/state", nil),
				httptest.NewRequest(http.MethodPost, "/v1/state/apply", strings.NewReader(stateApplyBody(`[]`))),
			} {
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				if rec.Code != http.StatusServiceUnavailable {
					t.Fatalf("%s %s: status = %d, want 503", req.Method, req.URL.Path, rec.Code)
				}
				var payload struct {
					Errors []validationError `json:"errors"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
					t.Fatalf("body is not JSON: %v", err)
				}
				if len(payload.Errors) != 1 || payload.Errors[0].Code != codeStateUnavailable {
					t.Fatalf("%s %s: errors = %+v, want state_unavailable", req.Method, req.URL.Path, payload.Errors)
				}
			}
		})
	}
}

func TestGetStateMissingFileReadsAsRevisionZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	res := doGetState(t, HandlerWithProviderAndStateFile(nil, path))
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (errors %v)", res.status, res.Errors)
	}
	if res.Revision == nil || *res.Revision != 0 {
		t.Fatalf("revision = %v, want 0", res.Revision)
	}
	if res.State == nil || len(res.State.Resources) != 0 {
		t.Fatalf("state = %+v, want empty resources", res.State)
	}
}

func TestStateApplyCreatesAndPersistsRevision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	provider := &recordingProvider{}
	handler := HandlerWithProviderAndStateFile(provider, path)

	body := stateApplyBody(`[` + resEntry("a", `["b"]`) + `,` + resEntry("b", `[]`) + `]`)
	res := doStateApply(t, handler, body)
	if res.status != http.StatusOK || !res.Valid || len(res.Errors) != 0 {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
	if res.Revision == nil || *res.Revision != 1 {
		t.Fatalf("revision = %v, want 1", res.Revision)
	}
	if res.Applied == nil || len(*res.Applied) != 2 {
		t.Fatalf("applied = %v, want 2 entries", res.Applied)
	}
	if got := providerCallsSummary(provider.calls); got != "create:b|create:a" {
		t.Fatalf("provider calls = %q, want create:b|create:a", got)
	}

	rev, resources := stateFileContents(t, path)
	if rev != 1 {
		t.Fatalf("file revision = %d, want 1", rev)
	}
	if got := fileAddresses(resources); got != "a|b" {
		t.Fatalf("file resources = %q, want a|b", got)
	}

	get := doGetState(t, handler)
	if get.Revision == nil || *get.Revision != 1 {
		t.Fatalf("GET revision = %v, want 1", get.Revision)
	}
	if got := fileAddresses(get.State.Resources); got != "a|b" {
		t.Fatalf("GET state = %q, want a|b", got)
	}
}

func TestStateApplyEmptyAndNoopPlansKeepRevision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	handler := HandlerWithProviderAndStateFile(&recordingProvider{}, path)

	// Empty plan against a missing file: success, revision 0, no file written.
	res := doStateApply(t, handler, stateApplyBody(`[]`))
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("empty plan: status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
	if res.Revision == nil || *res.Revision != 0 {
		t.Fatalf("empty plan revision = %v, want 0", res.Revision)
	}
	if res.Applied == nil || len(*res.Applied) != 0 {
		t.Fatalf("empty plan applied = %v, want empty", res.Applied)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("empty plan must not create the state file, stat err = %v", err)
	}

	// Bring the file to revision 1, then run an all-noop plan against it.
	create := doStateApply(t, handler, stateApplyBody(`[`+resEntry("a", `[]`)+`]`))
	if create.status != http.StatusOK {
		t.Fatalf("create status = %d", create.status)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	noop := doStateApply(t, handler, stateApplyBody(`[`+resEntry("a", `[]`)+`]`))
	if noop.status != http.StatusOK || noop.Revision == nil || *noop.Revision != 1 {
		t.Fatalf("noop plan: status=%d revision=%v, want 200 and revision 1", noop.status, noop.Revision)
	}
	if noop.Summary == nil || *noop.Summary != (planSummary{Noop: 1}) {
		t.Fatalf("noop summary = %+v", noop.Summary)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("noop plan must not rewrite the file\nbefore: %s\nafter:  %s", before, after)
	}
}

func TestStateApplyExpectedRevision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	provider := &recordingProvider{}
	handler := HandlerWithProviderAndStateFile(provider, path)

	// Missing file is revision 0; matching expectedRevision lets the run through.
	body := stateApplyBody(`[` + resEntry("a", `[]`) + `]`)
	body = body[:len(body)-1] + `, "expectedRevision": 0}`
	res := doStateApply(t, handler, body)
	if res.status != http.StatusOK || res.Revision == nil || *res.Revision != 1 {
		t.Fatalf("matching expectedRevision: status=%d revision=%v errors=%v", res.status, res.Revision, res.Errors)
	}

	// Stale expectedRevision conflicts, reports currentRevision, and the
	// provider is not called again.
	callsBefore := len(provider.calls)
	stale := stateApplyBody(`[]`)
	stale = stale[:len(stale)-1] + `, "expectedRevision": 0}`
	conflict := doStateApply(t, handler, stale)
	if conflict.status != http.StatusConflict {
		t.Fatalf("stale expectedRevision: status = %d, want 409", conflict.status)
	}
	if len(conflict.Errors) != 1 || conflict.Errors[0].Code != codeStateConflict {
		t.Fatalf("conflict errors = %+v, want state_conflict", conflict.Errors)
	}
	if conflict.CurrentRevision == nil || *conflict.CurrentRevision != 1 {
		t.Fatalf("currentRevision = %v, want 1", conflict.CurrentRevision)
	}
	if len(provider.calls) != callsBefore {
		t.Fatalf("provider must not be called on conflict, got %v", providerCallsSummary(provider.calls))
	}
	if rev, _ := stateFileContents(t, path); rev != 1 {
		t.Fatalf("file revision = %d, want unchanged 1", rev)
	}
}

func TestStateApplyEnvelopeErrors(t *testing.T) {
	handler := HandlerWithProviderAndStateFile(&recordingProvider{}, filepath.Join(t.TempDir(), "state.json"))
	cases := map[string]string{
		"not an object":         `[]`,
		"missing configuration": `{"expectedRevision": 0}`,
		"unknown field":         `{"configuration": {"resourceTypes": [], "resources": []}, "priorState": {"resources": []}}`,
		"negative revision":     `{"configuration": {"resourceTypes": [], "resources": []}, "expectedRevision": -1}`,
		"fractional revision":   `{"configuration": {"resourceTypes": [], "resources": []}, "expectedRevision": 1.5}`,
		"string revision":       `{"configuration": {"resourceTypes": [], "resources": []}, "expectedRevision": "1"}`,
		"null revision":         `{"configuration": {"resourceTypes": [], "resources": []}, "expectedRevision": null}`,
		"boolean revision":      `{"configuration": {"resourceTypes": [], "resources": []}, "expectedRevision": true}`,
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
					t.Fatalf("error code = %q, want invalid_state_request (%+v)", e.Code, e)
				}
			}
		})
	}
}

func TestStateApplyInvalidJSONIs400(t *testing.T) {
	provider := &recordingProvider{}
	handler := HandlerWithProviderAndStateFile(provider, filepath.Join(t.TempDir(), "state.json"))
	for _, body := range []string{"", `{`, stateApplyBody(`[]`) + ` {}`} {
		res := doStateApply(t, handler, body)
		if res.status != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", res.status)
		}
		if len(res.Errors) != 1 || res.Errors[0].Code != codeInvalidJSON {
			t.Fatalf("payload = %+v", res)
		}
	}
	if len(provider.calls) != 0 {
		t.Fatalf("provider must not be called, got %v", provider.calls)
	}
}

func TestStateApplyConfigurationErrorsKeepPlanShape(t *testing.T) {
	provider := &recordingProvider{}
	handler := HandlerWithProviderAndStateFile(provider, filepath.Join(t.TempDir(), "state.json"))
	body := `{"configuration": {"resourceTypes": [], "resources": [{"address": "a", "type": "x", "properties": {}}]}}`
	res := doStateApply(t, handler, body)
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeUnknownResourceType {
		t.Fatalf("errors = %+v, want unknown_resource_type", res.Errors)
	}
	if !strings.HasPrefix(res.Errors[0].Path, "/configuration") {
		t.Fatalf("path = %q, want /configuration prefix", res.Errors[0].Path)
	}
	if len(provider.calls) != 0 {
		t.Fatalf("provider must not be called, got %v", provider.calls)
	}
}

func TestStateReadError(t *testing.T) {
	cases := map[string]string{
		"malformed JSON":      `{not json`,
		"empty file":          ``,
		"negative revision":   `{"revision": -1, "state": {"resources": []}}`,
		"fractional revision": `{"revision": 1.5, "state": {"resources": []}}`,
		"missing revision":    `{"state": {"resources": []}}`,
		"missing state":       `{"revision": 0}`,
		"invalid resources":   `{"revision": 0, "state": {"resources": [{}]}}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			handler := HandlerWithProviderAndStateFile(&recordingProvider{}, path)

			get := doGetState(t, handler)
			if get.status != http.StatusInternalServerError {
				t.Fatalf("GET status = %d, want 500", get.status)
			}
			if len(get.Errors) != 1 || get.Errors[0].Code != codeStateReadError {
				t.Fatalf("GET errors = %+v, want state_read_error", get.Errors)
			}

			apply := doStateApply(t, handler, stateApplyBody(`[`+resEntry("a", `[]`)+`]`))
			if apply.status != http.StatusInternalServerError {
				t.Fatalf("apply status = %d, want 500", apply.status)
			}
			if len(apply.Errors) != 1 || apply.Errors[0].Code != codeStateReadError {
				t.Fatalf("apply errors = %+v, want state_read_error", apply.Errors)
			}

			// The invalid file must not be rewritten.
			data, err := os.ReadFile(path)
			if err != nil || string(data) != content {
				t.Fatalf("file changed after read error: %q, err %v", data, err)
			}
		})
	}
}

func TestStateApplyChangesWithoutProviderIsUnavailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	res := doStateApply(t, HandlerWithProviderAndStateFile(nil, path), stateApplyBody(`[`+resEntry("a", `[]`)+`]`))
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeProviderUnavailable {
		t.Fatalf("errors = %+v, want provider_unavailable", res.Errors)
	}
	if _, present := res.raw["applied"]; present {
		t.Fatalf("503 response must not contain applied: %v", res.raw)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("no state file must be written, stat err = %v", err)
	}
}

func TestStateApplyProviderFirstFailureLeavesFileUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	provider := &recordingProvider{failAt: 0, failErr: errors.New("nope")}
	res := doStateApply(t, HandlerWithProviderAndStateFile(provider, path),
		stateApplyBody(`[`+resEntry("a", `[]`)+`]`))
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeProviderError || res.Errors[0].Path != "/changes/0" {
		t.Fatalf("errors = %+v", res.Errors)
	}
	if res.Applied == nil || len(*res.Applied) != 0 {
		t.Fatalf("applied = %v, want empty", res.Applied)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("first-change failure must leave no file, stat err = %v", err)
	}
}

func TestStateApplyProviderPartialFailurePersistsPartialState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	provider := &recordingProvider{failAt: 1, failErr: errors.New("boom at a")}
	res := doStateApply(t, HandlerWithProviderAndStateFile(provider, path),
		stateApplyBody(`[`+resEntry("a", `["b"]`)+`,`+resEntry("b", `[]`)+`]`))
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if res.Revision == nil || *res.Revision != 1 {
		t.Fatalf("revision = %v, want 1 (partial state committed)", res.Revision)
	}
	if res.Applied == nil || len(*res.Applied) != 1 || (*res.Applied)[0].Address != "b" {
		t.Fatalf("applied = %v, want only b", res.Applied)
	}
	rev, resources := stateFileContents(t, path)
	if rev != 1 {
		t.Fatalf("file revision = %d, want 1", rev)
	}
	if got := fileAddresses(resources); got != "b" {
		t.Fatalf("file resources = %q, want only b", got)
	}
}

func TestStateApplyWriteError(t *testing.T) {
	// The state file's directory does not exist, so the atomic commit fails.
	path := filepath.Join(t.TempDir(), "missing", "state.json")
	provider := &recordingProvider{}
	res := doStateApply(t, HandlerWithProviderAndStateFile(provider, path),
		stateApplyBody(`[`+resEntry("a", `[]`)+`]`))
	if res.status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeStateWriteError {
		t.Fatalf("errors = %+v, want state_write_error", res.Errors)
	}
	// The response keeps the executed changes and the pending state.
	if res.Applied == nil || len(*res.Applied) != 1 {
		t.Fatalf("applied = %v, want the executed change", res.Applied)
	}
	if res.State == nil || len(res.State.Resources) != 1 || res.State.Resources[0].Address != "a" {
		t.Fatalf("state = %+v, want pending state with a", res.State)
	}
	if res.Revision == nil || *res.Revision != 0 {
		t.Fatalf("revision = %v, want the uncommitted 0", res.Revision)
	}
}

func TestStateApplyWriteErrorKeepsOriginalFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-based commit failure does not apply to root")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	original := `{"revision":5,"state":{"resources":[{"address":"x","type":"t","properties":{},"dependsOn":[]}]}}`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o755) }()

	res := doStateApply(t, HandlerWithProviderAndStateFile(&recordingProvider{}, path),
		stateApplyBody(`[`+resEntry("a", `[]`)+`]`))
	if res.status != http.StatusInternalServerError || len(res.Errors) != 1 || res.Errors[0].Code != codeStateWriteError {
		t.Fatalf("status=%d errors=%+v, want 500 state_write_error", res.status, res.Errors)
	}

	// The pre-existing file is still complete and readable, and no temporary
	// file is left behind.
	data, err := os.ReadFile(path)
	if err != nil || string(data) != original {
		t.Fatalf("original file damaged: %q, err %v", data, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "state.json" {
			t.Fatalf("unexpected leftover file %q", entry.Name())
		}
	}
}

// blockingProvider blocks inside Apply until released, so a concurrent apply
// run observes the held lock.
type blockingProvider struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *blockingProvider) Apply(req ChangeRequest) error {
	p.once.Do(func() { close(p.started) })
	<-p.release
	return nil
}

func TestStateApplyLockExcludesSeparateHandlers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	blocker := &blockingProvider{started: make(chan struct{}), release: make(chan struct{})}
	first := HandlerWithProviderAndStateFile(blocker, path)
	// A separately created handler for the same file must observe the lock.
	second := HandlerWithProviderAndStateFile(&recordingProvider{}, path)

	body := stateApplyBody(`[` + resEntry("a", `[]`) + `]`)
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		first.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/state/apply", strings.NewReader(body)))
		firstDone <- rec
	}()
	<-blocker.started

	locked := doStateApply(t, second, body)
	if locked.status != http.StatusConflict {
		t.Fatalf("concurrent apply: status = %d, want 409", locked.status)
	}
	if len(locked.Errors) != 1 || locked.Errors[0].Code != codeStateLocked {
		t.Fatalf("concurrent apply: errors = %+v, want state_locked", locked.Errors)
	}

	close(blocker.release)
	firstRes := decodeStateApply(t, <-firstDone)
	if firstRes.status != http.StatusOK {
		t.Fatalf("first apply: status = %d, want 200", firstRes.status)
	}

	// The lock is released after the run: a follow-up apply proceeds.
	followUp := doStateApply(t, second, stateApplyBody(`[]`))
	if followUp.status != http.StatusOK {
		t.Fatalf("follow-up apply: status = %d, want 200 (lock released)", followUp.status)
	}
}

func TestStateApplyLockReleasedAfterFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	handler := HandlerWithProviderAndStateFile(
		&recordingProvider{failAt: 0, failErr: errors.New("boom")}, path)
	res := doStateApply(t, handler, stateApplyBody(`[`+resEntry("a", `[]`)+`]`))
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	again := doStateApply(t, HandlerWithProviderAndStateFile(&recordingProvider{}, path), stateApplyBody(`[]`))
	if again.status != http.StatusOK {
		t.Fatalf("after failure: status = %d, want 200 (lock released)", again.status)
	}
}

func TestStateApplyLockIsPerNormalizedPath(t *testing.T) {
	dir := t.TempDir()
	blocker := &blockingProvider{started: make(chan struct{}), release: make(chan struct{})}
	first := HandlerWithProviderAndStateFile(blocker, filepath.Join(dir, "one.json"))
	other := HandlerWithProviderAndStateFile(&recordingProvider{}, filepath.Join(dir, "two.json"))

	done := make(chan struct{})
	go func() {
		defer close(done)
		rec := httptest.NewRecorder()
		first.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/state/apply",
			strings.NewReader(stateApplyBody(`[`+resEntry("a", `[]`)+`]`))))
	}()
	<-blocker.started

	// A different path is not blocked.
	res := doStateApply(t, other, stateApplyBody(`[]`))
	if res.status != http.StatusOK {
		t.Fatalf("different path: status = %d, want 200", res.status)
	}
	close(blocker.release)
	<-done
}

func TestStateEndpointsRejectOtherMethods(t *testing.T) {
	handler := HandlerWithProviderAndStateFile(&recordingProvider{}, filepath.Join(t.TempDir(), "state.json"))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/state", strings.NewReader(`{}`)))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("POST /v1/state: status=%d Allow=%q, want 405 GET", rec.Code, rec.Header().Get("Allow"))
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/state/apply", nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("GET /v1/state/apply: status=%d Allow=%q, want 405 POST", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestStateHandlerKeepsExistingEndpoints(t *testing.T) {
	handler := HandlerWithProviderAndStateFile(&recordingProvider{}, filepath.Join(t.TempDir(), "state.json"))

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("healthz status = %d", health.Code)
	}

	plan := httptest.NewRecorder()
	handler.ServeHTTP(plan, httptest.NewRequest(http.MethodPost, "/v1/plans",
		strings.NewReader(planBody(`[`+resEntry("a", `[]`)+`]`, `[]`))))
	if plan.Code != http.StatusOK {
		t.Fatalf("plans status = %d", plan.Code)
	}

	// The stateless apply endpoint is unaffected by the state file.
	apply := doApply(t, handler, planBody(`[`+resEntry("a", `[]`)+`]`, `[]`), nil)
	if apply.status != http.StatusOK {
		t.Fatalf("apply status = %d", apply.status)
	}
}
