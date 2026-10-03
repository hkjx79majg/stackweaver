package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// Error codes reported by the state-file endpoints: the read-only GET and
// the locked, file-backed apply run.
const (
	codeStateUnavailable    = "state_unavailable"
	codeInvalidStateRequest = "invalid_state_request"
	codeStateReadError      = "state_read_error"
	codeStateWriteError     = "state_write_error"
	codeStateLocked         = "state_locked"
	codeStateConflict       = "state_conflict"
)

// stateBackend serves the state-file endpoints for one canonicalized file
// path. The file holds a non-negative integer revision and a state document
// in the same resources shape the plan endpoints use; a missing file reads
// as revision 0 with no resources.
type stateBackend struct {
	path string
}

// newStateBackend canonicalizes path once so that handlers created
// separately for the same file share one lock slot.
func newStateBackend(path string) *stateBackend {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return &stateBackend{path: resolved}
	}
	// The file itself may not exist yet; resolve through its directory.
	if resolvedDir, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		return &stateBackend{path: filepath.Join(resolvedDir, filepath.Base(abs))}
	}
	return &stateBackend{path: abs}
}

// stateLocks is the process-wide registry of state files currently being
// applied to, keyed by canonical path, so handlers created independently
// for the same path still exclude each other.
var stateLocks = struct {
	mu   sync.Mutex
	held map[string]bool
}{held: map[string]bool{}}

// tryLockState takes the non-blocking exclusive lock for path. It returns
// ok=false immediately when another apply run holds the lock; otherwise the
// returned function releases it and must be called on every outcome.
func tryLockState(path string) (unlock func(), ok bool) {
	stateLocks.mu.Lock()
	defer stateLocks.mu.Unlock()
	if stateLocks.held[path] {
		return nil, false
	}
	stateLocks.held[path] = true
	return func() {
		stateLocks.mu.Lock()
		delete(stateLocks.held, path)
		stateLocks.mu.Unlock()
	}, true
}

// stateFileContents is the validated view of one state file.
type stateFileContents struct {
	revision  int64
	resources []planResource
	order     []string
}

// errInvalidStateContent marks a file that exists but does not hold a legal
// state document; both unreadable files and invalid content surface as
// state_read_error and never cause a rewrite.
var errInvalidStateContent = fmt.Errorf("invalid state file content")

// readStateFile loads and validates the state file. A missing file is not
// an error: it reads as revision 0 with empty resources.
func readStateFile(path string) (stateFileContents, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return stateFileContents{revision: 0}, nil
		}
		return stateFileContents{}, err
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return stateFileContents{}, errInvalidStateContent
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return stateFileContents{}, errInvalidStateContent
	}

	obj, ok := doc.(map[string]any)
	if !ok {
		return stateFileContents{}, errInvalidStateContent
	}
	revision, ok := nonNegativeInt64(obj["revision"])
	if !ok {
		return stateFileContents{}, errInvalidStateContent
	}
	stateRaw, hasState := obj["state"]
	if !hasState {
		return stateFileContents{}, errInvalidStateContent
	}
	// The stored state document follows the same resources structure as the
	// plan envelope's priorState, so it is validated by the same rules.
	resources, order, errs := checkPriorState(stateRaw)
	if len(errs) != 0 {
		return stateFileContents{}, errInvalidStateContent
	}
	return stateFileContents{revision: revision, resources: resources, order: order}, nil
}

// nonNegativeInt64 extracts a non-negative integer from a decoded JSON
// value. Whole-valued forms such as 1.0 or 1e2 count as integers, matching
// the configuration validator's notion of integer.
func nonNegativeInt64(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	s := n.String()
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i, i >= 0
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsInf(f, 0) || f != math.Trunc(f) || f < 0 || f >= 9223372036854775808.0 {
		return 0, false
	}
	return int64(f), true
}

// writeStateFile atomically replaces the state file: the new document is
// written to a sibling temporary file, synced, and renamed over the target,
// so a failed commit never leaves a half-written file and an existing
// original stays intact and readable.
func writeStateFile(path string, revision int64, state map[string]planResource) error {
	doc := struct {
		Revision int64          `json:"revision"`
		State    *stateDocument `json:"state"`
	}{Revision: revision, State: renderState(state)}
	data, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".stackweaver-state-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Best-effort cleanup; after a successful rename this is a no-op.
	defer os.Remove(tmpName)

	if info, err := os.Stat(path); err == nil {
		// Preserve the existing file's permissions across the replacement.
		_ = tmp.Chmod(info.Mode().Perm())
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// handleStateUnavailable answers the state endpoints of handlers wired
// without a state file: every method gets 503 state_unavailable.
func handleStateUnavailable(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	writeValidation(w, http.StatusServiceUnavailable, []validationError{{
		Code:    codeStateUnavailable,
		Message: "state endpoints require a state file; use HandlerWithProviderAndStateFile",
		Path:    "",
	}})
}

// stateResponse is the GET /v1/state payload.
type stateResponse struct {
	Revision int64          `json:"revision"`
	State    *stateDocument `json:"state"`
}

// handleRead serves GET /v1/state: the current revision and the stored
// state document, normalized like an apply run's output state.
func (b *stateBackend) handleRead(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"code": "method_not_allowed"},
		})
		return
	}

	current, err := readStateFile(b.path)
	if err != nil {
		writeValidation(w, http.StatusInternalServerError, []validationError{{
			Code:    codeStateReadError,
			Message: "state file cannot be read or does not hold a valid state document",
			Path:    "",
		}})
		return
	}

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(stateResponse{
		Revision: current.revision,
		State:    renderState(seedState(current.resources)),
	})
}

// stateApplyResponse is the POST /v1/state/apply payload. Revision reports
// the file's revision after the run (unchanged when nothing was committed);
// CurrentRevision is set only on an expectedRevision conflict.
type stateApplyResponse struct {
	Valid           bool              `json:"valid"`
	Errors          []validationError `json:"errors"`
	Applied         *[]planChange     `json:"applied,omitempty"`
	Summary         planSummary       `json:"summary"`
	State           *stateDocument    `json:"state,omitempty"`
	Revision        *int64            `json:"revision,omitempty"`
	CurrentRevision *int64            `json:"currentRevision,omitempty"`
}

// handleApply serves POST /v1/state/apply: it plans the submitted
// configuration against the state file and executes under the file's
// non-blocking exclusive lock, committing the evolved state atomically.
func (b *stateBackend) handleApply(source providerSource) http.HandlerFunc {
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
		configRaw, expectedRevision, ok := checkStateApplyEnvelope(w, doc)
		if !ok {
			return
		}

		// The configuration is validated exactly like the plan endpoint's,
		// with findings rooted at /configuration.
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

		// The lock spans the run from read to commit; every outcome below
		// releases it via the deferred unlock.
		unlock, acquired := tryLockState(b.path)
		if !acquired {
			writeValidation(w, http.StatusConflict, []validationError{{
				Code:    codeStateLocked,
				Message: "state file is locked by another apply run",
				Path:    "",
			}})
			return
		}
		defer unlock()

		current, err := readStateFile(b.path)
		if err != nil {
			writeValidation(w, http.StatusInternalServerError, []validationError{{
				Code:    codeStateReadError,
				Message: "state file cannot be read or does not hold a valid state document",
				Path:    "",
			}})
			return
		}

		if expectedRevision != nil && *expectedRevision != current.revision {
			currentRevision := current.revision
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(stateApplyResponse{
				Valid: false,
				Errors: []validationError{{
					Code:    codeStateConflict,
					Message: "expectedRevision does not match the current state revision",
					Path:    "/expectedRevision",
				}},
				CurrentRevision: &currentRevision,
			})
			return
		}

		changes, summary := computePlan(configResources, configOrder, current.resources, current.order)
		state := seedState(current.resources)

		if len(changes) == 0 {
			// An empty plan succeeds without a Provider and leaves the file
			// and its revision untouched.
			revision := current.revision
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(stateApplyResponse{
				Valid:    true,
				Errors:   []validationError{},
				Applied:  &[]planChange{},
				Summary:  summary,
				State:    renderState(state),
				Revision: &revision,
			})
			return
		}

		// Precheck: every change must route to a registered Provider before
		// anything executes. An unroutable change aborts the whole run
		// without calling any Provider and leaves the file and its revision
		// untouched.
		providers := make([]Provider, len(changes))
		for i, change := range changes {
			typ := changeType(change)
			provider, registered := source.providerFor(typ)
			if !registered {
				revision := current.revision
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(stateApplyResponse{
					Valid:    false,
					Errors:   []validationError{source.applyUnavailable(typ, i)},
					Summary:  summary,
					Revision: &revision,
				})
				return
			}
			providers[i] = provider
		}

		applied := []planChange{}
		var providerErr error
		var failIndex int
		for i, change := range changes {
			req := ChangeRequest{
				Context: r.Context(),
				Action:  change.Action,
				Address: change.Address,
				Before:  change.Before,
				After:   change.After,
			}
			if err := providers[i].Apply(req); err != nil {
				// Stop immediately: later changes are skipped, earlier ones
				// are not rolled back.
				providerErr = err
				failIndex = i
				break
			}
			applied = append(applied, change)
			applyToState(state, change)
		}

		// Commit when the run has something to persist: every change
		// succeeded, or a Provider failure left at least one success behind.
		// A first-change failure keeps the file untouched.
		committed := false
		if providerErr == nil || len(applied) > 0 {
			if err := writeStateFile(b.path, current.revision+1, state); err != nil {
				// The commit failed; the file keeps its old revision. The
				// response still carries the executed changes and the state
				// that could not be persisted.
				revision := current.revision
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(stateApplyResponse{
					Valid: false,
					Errors: []validationError{{
						Code:    codeStateWriteError,
						Message: "state file could not be replaced atomically",
						Path:    "",
					}},
					Applied:  &applied,
					Summary:  summary,
					State:    renderState(state),
					Revision: &revision,
				})
				return
			}
			committed = true
		}

		if providerErr != nil {
			revision := current.revision
			if committed {
				// Partial state was persisted and takes exactly one version
				// increment.
				revision = current.revision + 1
			}
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(stateApplyResponse{
				Valid:    false,
				Errors:   []validationError{{Code: codeProviderError, Message: providerErr.Error(), Path: fmt.Sprintf("/changes/%d", failIndex)}},
				Applied:  &applied,
				Summary:  summary,
				State:    renderState(state),
				Revision: &revision,
			})
			return
		}

		revision := current.revision + 1
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(stateApplyResponse{
			Valid:    true,
			Errors:   []validationError{},
			Applied:  &applied,
			Summary:  summary,
			State:    renderState(state),
			Revision: &revision,
		})
	}
}

// checkStateApplyEnvelope validates the state-apply request envelope: a JSON
// object holding exactly the required configuration field and an optional
// expectedRevision that must be a non-negative integer. On failure it writes
// the 422 invalid_state_request response and returns ok=false.
func checkStateApplyEnvelope(w http.ResponseWriter, doc any) (configuration any, expectedRevision *int64, ok bool) {
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
		if revRaw, has := root["expectedRevision"]; has {
			if revision, valid := nonNegativeInt64(revRaw); valid {
				expectedRevision = &revision
			} else {
				add(codeInvalidStateRequest, `"expectedRevision" must be a non-negative integer`, "/expectedRevision")
			}
		}
	}

	if len(errs) != 0 {
		sortValidationErrors(errs)
		writeValidation(w, http.StatusUnprocessableEntity, errs)
		return nil, nil, false
	}
	return configuration, expectedRevision, true
}
