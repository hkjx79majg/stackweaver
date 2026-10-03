package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
)

// Error code reported by the state reconciliation endpoint when the request
// envelope is structurally unacceptable.
const codeInvalidReconcileRequest = "invalid_reconcile_request"

// reconcileResponse is the POST /v1/state/reconcile payload. A reconcile run
// never writes the state file, so Revision is always the revision read for the
// run and State is the stored state as read. CurrentRevision is set only on an
// expectedRevision conflict.
type reconcileResponse struct {
	Valid           bool              `json:"valid"`
	Errors          []validationError `json:"errors"`
	Applied         *[]planChange     `json:"applied,omitempty"`
	Summary         planSummary       `json:"summary"`
	State           *stateDocument    `json:"state,omitempty"`
	Revision        *int64            `json:"revision,omitempty"`
	CurrentRevision *int64            `json:"currentRevision,omitempty"`
}

// handleReconcile serves POST /v1/state/reconcile: it converges the remote
// toward the stored state. Resources are observed in ascending address order
// reusing drift detection's snapshot validation, normalization, and
// equivalence; the resulting creates and updates execute in the saved state's
// deterministic topological order so dependencies run first. The run never
// writes the state file or bumps its revision; the lock spans reading,
// observing, and applying and is released on every outcome.
func (b *stateBackend) handleReconcile(observer Observer, provider Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.Method != http.MethodPost {
			rejectMethodNotAllowed(w)
			return
		}

		// Malformed or multiple JSON values share the 400 invalid_json
		// contract of every other JSON endpoint.
		doc, ok := decodeConfigurationBody(w, r)
		if !ok {
			return
		}
		expectedRevision, ok := checkReconcileEnvelope(w, doc)
		if !ok {
			return
		}

		// The Observer capability is required before any locking or I/O.
		if observer == nil {
			writeValidation(w, http.StatusServiceUnavailable, []validationError{{
				Code:    codeObserverUnavailable,
				Message: "reconciliation requires the provider to implement Observer",
				Path:    "",
			}})
			return
		}

		// The same non-blocking exclusive lock guards apply, drift, and
		// reconcile runs, spanning the read, observation, and execution.
		unlock, acquired := tryLockState(b.path)
		if !acquired {
			writeValidation(w, http.StatusConflict, []validationError{{
				Code:    codeStateLocked,
				Message: "state file is locked by another run",
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
			// Checked before Observe or Apply is ever called.
			currentRevision := current.revision
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(reconcileResponse{
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

		// The original state is reported verbatim on every successful and
		// execution-failure outcome, as read and normalized.
		originalState := renderState(seedState(current.resources))
		revision := current.revision

		// Observe address by address in ascending Unicode order; that order
		// indexes observer errors. An empty state never reaches the Observer.
		resources := make([]planResource, len(current.resources))
		copy(resources, current.resources)
		sort.Slice(resources, func(i, j int) bool { return resources[i].address < resources[j].address })

		var summary planSummary
		changeByAddress := make(map[string]planChange, len(resources))
		for i, resource := range resources {
			observed, observeErr := observer.Observe(r.Context(), resource.address)
			if observeErr != nil {
				// Stop before any Apply: no change is executed.
				writeValidation(w, http.StatusBadGateway, []validationError{{
					Code:    codeObserverError,
					Message: observeErr.Error(),
					Path:    fmt.Sprintf("/resources/%d", i),
				}})
				return
			}

			saved := snapshotOf(resource)
			if observed == nil {
				// Remote is missing: create from the saved snapshot; create
				// omits before.
				summary.Create++
				after := saved
				changeByAddress[resource.address] = planChange{
					Address: resource.address,
					Action:  "create",
					After:   &after,
				}
				continue
			}

			normalized, normalizeErr := normalizeObserved(observed)
			if normalizeErr != nil {
				writeValidation(w, http.StatusBadGateway, []validationError{{
					Code:    codeObserverInvalidResult,
					Message: normalizeErr.Error(),
					Path:    fmt.Sprintf("/resources/%d", i),
				}})
				return
			}

			if snapshotsEquivalent(saved, normalized) {
				summary.Noop++
				continue
			}
			summary.Update++
			// update's before is the observed snapshot, after the saved one.
			before := normalized
			after := saved
			changeByAddress[resource.address] = planChange{
				Address: resource.address,
				Action:  "update",
				Before:  &before,
				After:   &after,
			}
		}

		// Execute in the saved state's deterministic topological order so
		// dependencies run first; noops never reach the Provider. The change
		// map holds only creates and updates, so Delete is always 0.
		changes := make([]planChange, 0, summary.Create+summary.Update)
		for _, address := range current.order {
			if change, found := changeByAddress[address]; found {
				changes = append(changes, change)
			}
		}

		if len(changes) == 0 {
			// A pure noop (or empty-state) run never calls Apply and leaves
			// the file untouched.
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(reconcileResponse{
				Valid:    true,
				Errors:   []validationError{},
				Applied:  &[]planChange{},
				Summary:  summary,
				State:    originalState,
				Revision: &revision,
			})
			return
		}

		applied := []planChange{}
		for i, change := range changes {
			req := ChangeRequest{
				Context: r.Context(),
				Action:  change.Action,
				Address: change.Address,
				Before:  change.Before,
				After:   change.After,
			}
			if err := provider.Apply(req); err != nil {
				// Stop immediately without rollback; the state file is never
				// written, but successes and the full summary are retained.
				w.WriteHeader(http.StatusBadGateway)
				_ = json.NewEncoder(w).Encode(reconcileResponse{
					Valid:    false,
					Errors:   []validationError{{Code: codeProviderError, Message: err.Error(), Path: fmt.Sprintf("/changes/%d", i)}},
					Applied:  &applied,
					Summary:  summary,
					State:    originalState,
					Revision: &revision,
				})
				return
			}
			applied = append(applied, change)
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(reconcileResponse{
			Valid:    true,
			Errors:   []validationError{},
			Applied:  &applied,
			Summary:  summary,
			State:    originalState,
			Revision: &revision,
		})
	}
}

// checkReconcileEnvelope validates the reconcile request: a JSON object whose
// only accepted field is an optional non-negative-integer expectedRevision.
// On failure it writes the 422 invalid_reconcile_request response and returns
// ok=false.
func checkReconcileEnvelope(w http.ResponseWriter, doc any) (*int64, bool) {
	var errs []validationError
	add := func(code, message, path string) {
		errs = append(errs, validationError{Code: code, Message: message, Path: path})
	}

	root, isObject := doc.(map[string]any)
	var expectedRevision *int64
	if !isObject {
		add(codeInvalidReconcileRequest, "request body must be a JSON object", "")
	} else {
		for key := range root {
			if key != "expectedRevision" {
				add(codeInvalidReconcileRequest, fmt.Sprintf("unknown field %q", key), "/"+pointerEscape(key))
			}
		}
		if revRaw, has := root["expectedRevision"]; has {
			if revision, valid := nonNegativeInt64(revRaw); valid {
				expectedRevision = &revision
			} else {
				add(codeInvalidReconcileRequest, `"expectedRevision" must be a non-negative integer`, "/expectedRevision")
			}
		}
	}

	if len(errs) != 0 {
		sortValidationErrors(errs)
		writeValidation(w, http.StatusUnprocessableEntity, errs)
		return nil, false
	}
	return expectedRevision, true
}
