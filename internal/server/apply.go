package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
)

// Error code reported when an apply run has changes to execute but no
// Provider was injected, and when a Provider rejects a change.
const (
	codeProviderUnavailable = "provider_unavailable"
	codeProviderError       = "provider_error"
	codeRollbackError       = "rollback_error"
)

type stateDocument struct {
	Resources []stateResource `json:"resources"`
}

// stateResource is one entry of the state document returned by an apply run.
type stateResource struct {
	Address    string         `json:"address"`
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties"`
	DependsOn  []string       `json:"dependsOn"`
}

type applyResponse struct {
	Valid   bool              `json:"valid"`
	Errors  []validationError `json:"errors"`
	Applied *[]planChange     `json:"applied,omitempty"`
	Summary planSummary       `json:"summary"`
	State   *stateDocument    `json:"state,omitempty"`
	// RolledBack and RollbackErrors are present only on runs requested with
	// rollbackOnError=true that reach execution (200/502), even when empty.
	// Baseline runs and pre-execution aborts omit them, exactly like applied.
	RolledBack     *[]planChange      `json:"rolledBack,omitempty"`
	RollbackErrors *[]validationError `json:"rollbackErrors,omitempty"`
}

// handleApply validates and plans a request exactly like POST /v1/plans, then
// hands the ordered non-noop changes to the resolved Providers one at a time.
// It returns a handler so the injected provider source is captured once at
// wiring time.
func handleApply(source providerSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.Method != http.MethodPost {
			rejectMethodNotAllowed(w)
			return
		}

		changes, summary, priorResources, rollbackOnError, ok := preparePlan(w, r, true)
		if !ok {
			// preparePlan already wrote the 400/422 response; a Provider is
			// never called for an invalid request.
			return
		}

		// Precheck: every change must route to a registered Provider before
		// anything executes. An unroutable change aborts the whole run
		// without calling any Provider; an empty plan needs none. When
		// rollback is requested, each change's possible compensation is
		// prechecked as well, so compensation can never discover a missing
		// route after a forward change has landed.
		providers := make([]Provider, len(changes))
		for i, change := range changes {
			typ := changeType(change)
			provider, registered := source.providerFor(typ)
			if !registered {
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(applyResponse{
					Valid:   false,
					Errors:  []validationError{source.applyUnavailable(typ, i)},
					Summary: summary,
				})
				return
			}
			providers[i] = provider

			if rollbackOnError {
				rollbackType := changeType(compensationOf(change))
				if _, registered := source.providerFor(rollbackType); !registered {
					w.WriteHeader(http.StatusServiceUnavailable)
					_ = json.NewEncoder(w).Encode(applyResponse{
						Valid:   false,
						Errors:  []validationError{source.applyUnavailable(rollbackType, i)},
						Summary: summary,
					})
					return
				}
			}
		}

		// State starts from priorState and is mutated only as changes
		// succeed; the service holds it for this request alone.
		state := seedState(priorResources)
		applied := []planChange{}

		// One fresh idempotency key per non-noop change of this top-level
		// request; retries of a change reuse its key, and a later request
		// mints a new set. When rollback is requested a second, disjoint set
		// is minted up front, one per possible compensation; retries of one
		// compensation reuse its key, and every forward and compensation key
		// is unique within this request. Unused compensation keys never
		// reach a Provider.
		keys := mintIdempotencyKeys(len(changes))
		var rollbackKeys []string
		if rollbackOnError {
			rollbackKeys = mintAdditionalKeys(len(changes), keys)
		}

		failIndex := -1
		var forwardErr error
		for i, change := range changes {
			// computePlan only emits create/update/delete; noops are absent
			// from the list, so a Provider is never invoked for them.
			if err := executeChange(r.Context(), providers[i], change, keys[i]); err != nil {
				// Stop immediately: later changes are skipped. Without
				// rollback, earlier changes stay in place; with rollback the
				// successful changes are compensated below. Either way the
				// last error text is reported at the change's global index.
				failIndex = i
				forwardErr = err
				break
			}
			applied = append(applied, change)
			applyToState(state, change)
		}

		if failIndex == -1 {
			resp := applyResponse{
				Valid:   true,
				Errors:  []validationError{},
				Applied: &applied,
				Summary: summary,
				State:   renderState(state),
			}
			if rollbackOnError {
				// Every forward change succeeded: no compensation runs, but
				// the two arrays are always present on a rollback-enabled run
				// that reached execution.
				resp.RolledBack = &[]planChange{}
				resp.RollbackErrors = &[]validationError{}
			}
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		if !rollbackOnError {
			// Baseline partial-failure response: successful changes stay
			// applied and state keeps their effects.
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(applyResponse{
				Valid:   false,
				Errors:  []validationError{{Code: codeProviderError, Message: forwardErr.Error(), Path: fmt.Sprintf("/changes/%d", failIndex)}},
				Applied: &applied,
				Summary: summary,
				State:   renderState(state),
			})
			return
		}

		// Compensate the successful changes in reverse execution order. A
		// compensation failure is recorded against /rollbacks/{original
		// index} but never aborts the sweep: every earlier success still
		// gets its turn. rolledBack follows the actual compensation order.
		rolledBack := []planChange{}
		rollbackErrors := []validationError{}
		undone := map[int]bool{}
		for j := failIndex - 1; j >= 0; j-- {
			comp := compensationOf(changes[j])
			compProvider, _ := source.providerFor(changeType(comp))
			if err := executeChange(r.Context(), compProvider, comp, rollbackKeys[j]); err != nil {
				rollbackErrors = append(rollbackErrors, validationError{
					Code:    codeRollbackError,
					Message: err.Error(),
					Path:    fmt.Sprintf("/rollbacks/%d", j),
				})
				continue
			}
			undone[j] = true
			rolledBack = append(rolledBack, changes[j])
		}

		// applied keeps only the changes still in effect after compensation,
		// in the original execution order; state is the deterministic result
		// of applying those residual changes to the normalized prior state,
		// so a fully successful compensation reproduces the prior state.
		residual := []planChange{}
		finalState := seedState(priorResources)
		for j := 0; j < failIndex; j++ {
			if undone[j] {
				continue
			}
			residual = append(residual, changes[j])
			applyToState(finalState, changes[j])
		}

		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(applyResponse{
			Valid:          false,
			Errors:         []validationError{{Code: codeProviderError, Message: forwardErr.Error(), Path: fmt.Sprintf("/changes/%d", failIndex)}},
			Applied:        &residual,
			Summary:        summary,
			State:          renderState(finalState),
			RolledBack:     &rolledBack,
			RollbackErrors: &rollbackErrors,
		})
	}
}

// compensationOf inverts one planned change: create is undone with a delete
// carrying the created snapshot, delete with a create carrying the removed
// snapshot, and update with another update whose before and after are
// exchanged. The compensation routes by the type of the snapshot it acts
// toward, so a type-changing update is undone through the original type's
// Provider.
func compensationOf(change planChange) planChange {
	switch change.Action {
	case "create":
		return planChange{Address: change.Address, Action: "delete", Before: change.After}
	case "delete":
		return planChange{Address: change.Address, Action: "create", After: change.Before}
	default: // update
		return planChange{Address: change.Address, Action: "update", Before: change.After, After: change.Before}
	}
}

// seedState copies the validated prior-state resources into an addressable
// map that accumulates the effects of a run. Entries are normalized through
// snapshotOf so the output document is uniform: dependencies are sorted and
// always present, exactly like the after snapshots of successful changes.
func seedState(resources []planResource) map[string]planResource {
	state := make(map[string]planResource, len(resources))
	for _, resource := range resources {
		snap := snapshotOf(resource)
		state[resource.address] = planResource{
			address:    resource.address,
			typ:        snap.Type,
			properties: snap.Properties,
			dependsOn:  snap.DependsOn,
		}
	}
	return state
}

// applyToState records one successfully applied change: create and update
// adopt after, delete removes the address.
func applyToState(state map[string]planResource, change planChange) {
	switch change.Action {
	case "create", "update":
		state[change.Address] = planResource{
			address:    change.Address,
			typ:        change.After.Type,
			properties: change.After.Properties,
			dependsOn:  change.After.DependsOn,
		}
	case "delete":
		delete(state, change.Address)
	}
}

// renderState serializes the working state as an address-sorted document.
// Addresses are ordered by Unicode code point, which matches Go's byte-wise
// string comparison for UTF-8.
func renderState(state map[string]planResource) *stateDocument {
	addresses := make([]string, 0, len(state))
	for address := range state {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)

	resources := make([]stateResource, 0, len(addresses))
	for _, address := range addresses {
		resource := state[address]
		dependsOn := resource.dependsOn
		if dependsOn == nil {
			dependsOn = []string{}
		}
		resources = append(resources, stateResource{
			Address:    address,
			Type:       resource.typ,
			Properties: resource.properties,
			DependsOn:  dependsOn,
		})
	}
	return &stateDocument{Resources: resources}
}
