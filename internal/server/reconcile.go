package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
)

// Error code reported when the reconcile request envelope is structurally
// invalid: the body is not an object, names an unknown field, or carries an
// illegal expectedRevision.
const codeInvalidReconcileRequest = "invalid_reconcile_request"

// reconcileResponse is the POST /v1/state/reconcile payload. State and
// Revision always report the file contents the run reconciled against: the
// endpoint never writes and never bumps the revision. CurrentRevision is set
// only on an expectedRevision conflict.
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
// toward the stored state without persisting anything. The run observes every
// saved resource in ascending address order, classifies each as create,
// update, or noop against the saved snapshot, and then applies the changes in
// the saved state's deterministic topological order. It never writes the
// state file and never bumps the revision.
func (b *stateBackend) handleReconcile(router providerRouter) http.HandlerFunc {
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
		expectedRevision, ok := checkReconcileEnvelope(w, doc)
		if !ok {
			return
		}

		// In single-provider mode the Observer capability is required before
		// locking or I/O, exactly like drift detection.
		var singleObserver Observer
		if !router.multi {
			singleObserver = observerOf(router.single)
			if singleObserver == nil {
				writeValidation(w, http.StatusServiceUnavailable, []validationError{{
					Code:    codeObserverUnavailable,
					Message: "reconciliation requires the provider to implement Observer",
					Path:    "",
				}})
				return
			}
		}

		// One lock spans the read, every observation, and every Apply call;
		// every outcome below releases it via the deferred unlock.
		unlock, acquired := tryLockState(b.path)
		if !acquired {
			writeValidation(w, http.StatusConflict, []validationError{{
				Code:    codeStateLocked,
				Message: "state file is locked by another state run",
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

		// Observe in ascending address order; that order indexes observation
		// errors. The saved snapshot is the before of an update and the after
		// of every create/update.
		observed := make([]planResource, len(current.resources))
		copy(observed, current.resources)
		sort.Slice(observed, func(i, j int) bool { return observed[i].address < observed[j].address })

		observers := make([]Observer, len(observed))
		if router.multi {
			// Preflight every saved resource under the lock before observing
			// or applying anything: an unregistered type or a missing
			// Observer capability fails the run with no remote calls at all.
			for i, resource := range observed {
				provider := router.forType(resource.typ)
				if provider == nil {
					writeValidation(w, http.StatusServiceUnavailable, []validationError{{
						Code:    codeProviderUnavailable,
						Message: fmt.Sprintf("no provider registered for resource type %q", resource.typ),
						Path:    fmt.Sprintf("/resources/%d", i),
					}})
					return
				}
				observer := observerOf(provider)
				if observer == nil {
					writeValidation(w, http.StatusServiceUnavailable, []validationError{{
						Code:    codeObserverUnavailable,
						Message: fmt.Sprintf("provider for resource type %q does not implement Observer", resource.typ),
						Path:    fmt.Sprintf("/resources/%d", i),
					}})
					return
				}
				observers[i] = observer
			}
		} else {
			for i := range observers {
				observers[i] = singleObserver
			}
		}

		changes := []planChange{}
		var summary planSummary
		for i, resource := range observed {
			snapshot, observeErr := observers[i].Observe(r.Context(), resource.address)
			if observeErr != nil {
				// Stop before any change is made: nothing has been applied yet.
				writeValidation(w, http.StatusBadGateway, []validationError{{
					Code:    codeObserverError,
					Message: observeErr.Error(),
					Path:    fmt.Sprintf("/resources/%d", i),
				}})
				return
			}

			saved := snapshotOf(resource)
			if snapshot == nil {
				// The resource is saved but absent remotely: recreate it.
				summary.Create++
				after := saved
				changes = append(changes, planChange{Address: resource.address, Action: "create", After: &after})
				continue
			}

			normalized, normalizeErr := normalizeObserved(snapshot)
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
			before := normalized
			after := saved
			changes = append(changes, planChange{
				Address: resource.address,
				Action:  "update",
				Before:  &before,
				After:   &after,
			})
		}

		state := renderState(seedState(current.resources))
		revision := current.revision

		// Reorder the observed changes into the saved state's deterministic
		// topological order, so dependencies are converged first. Resources not
		// in that order (none expected from a validated file) sort last.
		changes = orderReconcileChanges(changes, current.order)

		// All noops: no Provider is ever invoked and the run succeeds even
		// when no Provider was injected.
		applied := []planChange{}
		if len(changes) > 0 {
			if !router.multi && router.single == nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(reconcileResponse{
					Valid: false,
					Errors: []validationError{{
						Code:    codeProviderUnavailable,
						Message: "apply requires a provider, but none is configured",
						Path:    "",
					}},
					Summary:  summary,
					State:    state,
					Revision: &revision,
				})
				return
			}

			for i, change := range changes {
				req := ChangeRequest{
					Context: r.Context(),
					Action:  change.Action,
					Address: change.Address,
					Before:  change.Before,
					After:   change.After,
				}
				if err := router.forChange(change).Apply(req); err != nil {
					// Stop immediately without rolling back; successful
					// changes and the full summary are still reported.
					w.WriteHeader(http.StatusBadGateway)
					_ = json.NewEncoder(w).Encode(reconcileResponse{
						Valid:    false,
						Errors:   []validationError{{Code: codeProviderError, Message: err.Error(), Path: fmt.Sprintf("/changes/%d", i)}},
						Applied:  &applied,
						Summary:  summary,
						State:    state,
						Revision: &revision,
					})
					return
				}
				applied = append(applied, change)
			}
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(reconcileResponse{
			Valid:    true,
			Errors:   []validationError{},
			Applied:  &applied,
			Summary:  summary,
			State:    state,
			Revision: &revision,
		})
	}
}

// orderReconcileChanges re-expresses changes (observed in address order) in
// the saved state's topological order. The change set covers a subset of the
// saved addresses, so walking order and selecting by address yields the
// dependency-first execution order; any unmatched addresses follow in address
// order as a defensive fallback.
func orderReconcileChanges(changes []planChange, order []string) []planChange {
	byAddress := make(map[string]planChange, len(changes))
	for _, change := range changes {
		byAddress[change.Address] = change
	}
	ordered := make([]planChange, 0, len(changes))
	for _, address := range order {
		if change, ok := byAddress[address]; ok {
			ordered = append(ordered, change)
			delete(byAddress, address)
		}
	}
	if len(byAddress) != 0 {
		remaining := make([]string, 0, len(byAddress))
		for address := range byAddress {
			remaining = append(remaining, address)
		}
		sort.Strings(remaining)
		for _, address := range remaining {
			ordered = append(ordered, byAddress[address])
		}
	}
	return ordered
}

// checkReconcileEnvelope validates the reconcile request: a JSON object that
// may hold only an optional expectedRevision, which must be a non-negative
// integer. On failure it writes the 422 invalid_reconcile_request response
// and returns ok=false.
func checkReconcileEnvelope(w http.ResponseWriter, doc any) (expectedRevision *int64, ok bool) {
	var errs []validationError
	add := func(code, message, path string) {
		errs = append(errs, validationError{Code: code, Message: message, Path: path})
	}

	root, isObject := doc.(map[string]any)
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
