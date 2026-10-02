package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// Error codes reported by the state file backend.
const (
	codeStateUnavailable    = "state_unavailable"
	codeInvalidStateRequest = "invalid_state_request"
	codeStateReadError      = "state_read_error"
	codeStateWriteError     = "state_write_error"
	codeStateLocked         = "state_locked"
	codeStateConflict       = "state_conflict"
)

// stateFileDocument is the on-disk shape of the state file: a non-negative
// integer revision alongside a state document with the same resources
// structure the apply endpoints already return.
type stateFileDocument struct {
	Revision int64          `json:"revision"`
	State    *stateDocument `json:"state"`
}

// stateResponse is the GET /v1/state payload.
type stateResponse struct {
	Revision int64          `json:"revision"`
	State    *stateDocument `json:"state"`
}

// stateApplyResponse mirrors applyResponse and adds the revision the state
// file reached (or, on a commit failure, still holds).
type stateApplyResponse struct {
	Valid    bool              `json:"valid"`
	Errors   []validationError `json:"errors"`
	Applied  *[]planChange     `json:"applied,omitempty"`
	Summary  planSummary       `json:"summary"`
	State    *stateDocument    `json:"state,omitempty"`
	Revision *int64            `json:"revision,omitempty"`
}

// stateConflictResponse reports an expectedRevision mismatch together with
// the revision the state file is actually at.
type stateConflictResponse struct {
	Valid           bool              `json:"valid"`
	Errors          []validationError `json:"errors"`
	CurrentRevision int64             `json:"currentRevision"`
}

// errStateFileInvalid marks a state file whose content cannot be interpreted;
// like an unreadable file it surfaces as state_read_error.
var errStateFileInvalid = errors.New("state file content is invalid")

// stateLocks is the process-wide registry of held state-file locks, keyed by
// normalized path so handlers created separately for the same file still
// exclude each other.
var stateLocks = struct {
	sync.Mutex
	held map[string]struct{}
}{held: make(map[string]struct{})}

// normalizeStatePath reduces a state file path to its canonical absolute
// form so different spellings of one file share a single lock.
func normalizeStatePath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return abs
}

// acquireStateLock takes the non-blocking exclusive lock for a normalized
// path, reporting whether it was free.
func acquireStateLock(normalizedPath string) bool {
	stateLocks.Lock()
	defer stateLocks.Unlock()
	if _, busy := stateLocks.held[normalizedPath]; busy {
		return false
	}
	stateLocks.held[normalizedPath] = struct{}{}
	return true
}

func releaseStateLock(normalizedPath string) {
	stateLocks.Lock()
	delete(stateLocks.held, normalizedPath)
	stateLocks.Unlock()
}

// handleStateUnavailable serves the state endpoints of a handler that has no
// state file configured: every access is 503 state_unavailable.
func handleStateUnavailable(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	writeValidation(w, http.StatusServiceUnavailable, []validationError{{
		Code:    codeStateUnavailable,
		Message: "no state file is configured for this handler",
		Path:    "",
	}})
}

// handleGetState returns the current revision and state document of the
// state file. A missing file reads as revision 0 with empty resources.
func handleGetState(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{"code": "method_not_allowed"},
			})
			return
		}

		revision, resources, _, err := readStateFile(path)
		if err != nil {
			writeStateReadError(w)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(stateResponse{
			Revision: revision,
			State:    renderState(seedState(resources)),
		})
	}
}

// handleStateApply plans a configuration against the state file and executes
// the resulting changes, persisting the outcome back to the file. The
// normalized path's non-blocking exclusive lock is held from the read of the
// file until the commit, and released on every outcome.
func handleStateApply(provider Provider, path string) http.HandlerFunc {
	lockPath := normalizeStatePath(path)
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.Method != http.MethodPost {
			rejectMethodNotAllowed(w)
			return
		}

		doc, ok := decodeConfigurationBody(w, r)
		if !ok {
			return
		}
		configRaw, expectedRevision, hasExpected, ok := checkStateEnvelope(w, doc)
		if !ok {
			return
		}

		// The configuration is validated exactly like the plan endpoint
		// validates it; findings gain a /configuration path prefix.
		var errs []validationError
		var configResources []planResource
		var configOrder []string
		if baseErrs := validateConfiguration(configRaw); len(baseErrs) != 0 {
			errs = appendPrefixedErrors(errs, baseErrs, "/configuration")
		} else if order, depErrs := orderConfiguration(configRaw); len(depErrs) != 0 {
			errs = appendPrefixedErrors(errs, depErrs, "/configuration")
		} else {
			configResources = configurationResources(configRaw)
			configOrder = order
		}
		if len(errs) != 0 {
			sortValidationErrors(errs)
			writeValidation(w, http.StatusUnprocessableEntity, errs)
			return
		}

		if !acquireStateLock(lockPath) {
			writeValidation(w, http.StatusConflict, []validationError{{
				Code:    codeStateLocked,
				Message: "state file is locked by another apply run",
				Path:    "",
			}})
			return
		}
		defer releaseStateLock(lockPath)

		revision, stateResources, stateOrder, err := readStateFile(path)
		if err != nil {
			// A file that cannot be read is never rewritten.
			writeStateReadError(w)
			return
		}

		if hasExpected && expectedRevision != revision {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(stateConflictResponse{
				Valid: false,
				Errors: []validationError{{
					Code:    codeStateConflict,
					Message: fmt.Sprintf("expected revision %d, but the state file is at revision %d", expectedRevision, revision),
					Path:    "/expectedRevision",
				}},
				CurrentRevision: revision,
			})
			return
		}

		changes, summary := computePlan(configResources, configOrder, stateResources, stateOrder)

		if len(changes) > 0 && provider == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(applyResponse{
				Valid: false,
				Errors: []validationError{{
					Code:    codeProviderUnavailable,
					Message: "apply requires a provider, but none is configured",
					Path:    "",
				}},
				Summary: summary,
			})
			return
		}

		state := seedState(stateResources)
		applied := []planChange{}
		var providerErr error
		failedAt := 0
		for i, change := range changes {
			req := ChangeRequest{
				Context: r.Context(),
				Action:  change.Action,
				Address: change.Address,
				Before:  change.Before,
				After:   change.After,
			}
			if err := provider.Apply(req); err != nil {
				// Stop immediately: later changes are skipped, earlier ones
				// are not rolled back.
				providerErr = err
				failedAt = i
				break
			}
			applied = append(applied, change)
			applyToState(state, change)
		}

		// Exactly one revision increment is persisted when at least one
		// change was applied, fully or partially. An empty plan and a
		// first-change failure leave the file untouched.
		currentRevision := revision
		if len(applied) > 0 {
			if err := writeStateFileAtomic(path, revision+1, renderState(state)); err != nil {
				// A failed commit leaves no half-written file; the response
				// still carries the executed changes and the pending state.
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(stateApplyResponse{
					Valid:    false,
					Errors:   []validationError{{Code: codeStateWriteError, Message: "state file could not be committed", Path: ""}},
					Applied:  &applied,
					Summary:  summary,
					State:    renderState(state),
					Revision: &currentRevision,
				})
				return
			}
			currentRevision = revision + 1
		}

		if providerErr != nil {
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(stateApplyResponse{
				Valid:    false,
				Errors:   []validationError{{Code: codeProviderError, Message: providerErr.Error(), Path: fmt.Sprintf("/changes/%d", failedAt)}},
				Applied:  &applied,
				Summary:  summary,
				State:    renderState(state),
				Revision: &currentRevision,
			})
			return
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(stateApplyResponse{
			Valid:    true,
			Errors:   []validationError{},
			Applied:  &applied,
			Summary:  summary,
			State:    renderState(state),
			Revision: &currentRevision,
		})
	}
}

// checkStateEnvelope validates the state apply request envelope: a JSON
// object holding exactly the configuration field and an optional
// expectedRevision that must be a non-negative integer. On failure it writes
// the 422 invalid_state_request response and returns ok=false.
func checkStateEnvelope(w http.ResponseWriter, doc any) (configuration any, expectedRevision int64, hasExpected bool, ok bool) {
	var errs []validationError
	add := func(code, message, path string) {
		errs = append(errs, validationError{Code: code, Message: message, Path: path})
	}

	root, isObject := doc.(map[string]any)
	if !isObject {
		add(codeInvalidStateRequest, "request body must be a JSON object", "")
	} else {
		for key := range root {
			if key != "configuration" && key != "expectedRevision" {
				add(codeInvalidStateRequest, fmt.Sprintf("unknown field %q", key), "/"+pointerEscape(key))
			}
		}
		var hasConfig bool
		configuration, hasConfig = root["configuration"]
		if !hasConfig {
			add(codeInvalidStateRequest, `missing required field "configuration"`, "")
		}
		if raw, present := root["expectedRevision"]; present {
			num, isNumber := raw.(json.Number)
			revision, valid := int64(0), false
			if isNumber {
				revision, valid = parseRevision(num)
			}
			if !valid {
				add(codeInvalidStateRequest, `"expectedRevision" must be a non-negative integer`, "/expectedRevision")
			} else {
				expectedRevision, hasExpected = revision, true
			}
		}
	}

	if len(errs) != 0 {
		sortValidationErrors(errs)
		writeValidation(w, http.StatusUnprocessableEntity, errs)
		return nil, 0, false, false
	}
	return configuration, expectedRevision, hasExpected, true
}

// readStateFile loads the state file. A missing file reads as revision 0
// with no resources. Any other read failure or any structural problem —
// malformed JSON, a missing or negative or non-integer revision, an invalid
// resources document — is reported as an error and the caller must not
// rewrite the file.
func readStateFile(path string) (revision int64, resources []planResource, order []string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil, nil, nil
		}
		return 0, nil, nil, err
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return 0, nil, nil, errStateFileInvalid
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return 0, nil, nil, errStateFileInvalid
	}

	root, ok := doc.(map[string]any)
	if !ok {
		return 0, nil, nil, errStateFileInvalid
	}
	revisionRaw, hasRevision := root["revision"]
	revisionNum, isNumber := revisionRaw.(json.Number)
	if !hasRevision || !isNumber {
		return 0, nil, nil, errStateFileInvalid
	}
	revision, ok = parseRevision(revisionNum)
	if !ok {
		return 0, nil, nil, errStateFileInvalid
	}
	stateRaw, hasState := root["state"]
	if !hasState {
		return 0, nil, nil, errStateFileInvalid
	}
	resources, order, stateErrs := checkPriorState(stateRaw)
	if len(stateErrs) != 0 {
		return 0, nil, nil, errStateFileInvalid
	}
	return revision, resources, order, nil
}

// parseRevision interprets a JSON number as a non-negative integer revision,
// accepting whole-valued forms such as 1.0 or 1e2.
func parseRevision(n json.Number) (int64, bool) {
	if !isJSONInteger(n) {
		return 0, false
	}
	rat, ok := new(big.Rat).SetString(n.String())
	if !ok || !rat.IsInt() {
		return 0, false
	}
	num := rat.Num()
	if num.Sign() < 0 || !num.IsInt64() {
		return 0, false
	}
	return num.Int64(), true
}

// writeStateFileAtomic persists the document by writing a temporary file in
// the same directory and renaming it over the target, so a failure never
// leaves a half-written file and an existing file stays intact and readable.
func writeStateFileAtomic(path string, revision int64, state *stateDocument) error {
	data, err := json.Marshal(stateFileDocument{Revision: revision, State: state})
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".stackweaver-state-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// writeStateReadError writes the shared 500 response for an unreadable or
// invalid state file.
func writeStateReadError(w http.ResponseWriter) {
	writeValidation(w, http.StatusInternalServerError, []validationError{{
		Code:    codeStateReadError,
		Message: "state file is unreadable or holds invalid content",
		Path:    "",
	}})
}
