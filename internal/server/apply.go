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
	// RolledBack and RollbackErrors are present exactly when the request
	// enabled rollbackOnError; both are empty arrays unless a forward
	// failure triggered compensation.
	RolledBack     *[]planChange      `json:"rolledBack,omitempty"`
	RollbackErrors *[]validationError `json:"rollbackErrors,omitempty"`
}

// handleApply validates and plans a request exactly like POST /v1/plans, then
// hands the ordered non-noop changes to the resolved Providers one at a time.
// The envelope may additionally carry rollbackOnError: when true, a change
// that fails finally stops the run and the already-applied changes are
// compensated in reverse order. It returns a handler so the injected provider
// source is captured once at wiring time.
func handleApply(source providerSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.Method != http.MethodPost {
			rejectMethodNotAllowed(w)
			return
		}

		changes, summary, priorResources, rollbackOnError, ok := prepareApplyPlan(w, r)
		if !ok {
			// prepareApplyPlan already wrote the 400/422 response; a Provider
			// is never called for an invalid request.
			return
		}

		// Precheck: every change must route to a registered Provider before
		// anything executes. An unroutable change aborts the whole run
		// without calling any Provider; an empty plan needs none.
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
		}

		// When rollback is enabled, every compensation route is prechecked
		// too: any change may need compensating once a later change fails, so
		// an unroutable compensation aborts the run exactly like an
		// unroutable forward change, indexed at the original change.
		var compensations []planChange
		var compensationProviders []Provider
		if rollbackOnError {
			compensations = make([]planChange, len(changes))
			compensationProviders = make([]Provider, len(changes))
			for i, change := range changes {
				compensation := compensationFor(change)
				compensations[i] = compensation
				typ := changeType(compensation)
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
				compensationProviders[i] = provider
			}
		}

		// State starts from priorState and is mutated only as changes
		// succeed; the service holds it for this request alone.
		state := seedState(priorResources)
		applied := []planChange{}
		// appliedIndex maps each applied entry back to its plan index, so a
		// compensation can be routed, keyed, and reported against the
		// original change.
		appliedIndex := []int{}

		// One fresh idempotency key per non-noop change of this top-level
		// request; retries of a change reuse its key, and a later request
		// mints a new set. With rollback enabled a second, disjoint set is
		// minted up front for the possible compensations, so every key of
		// the request — forward or compensation — is pairwise distinct.
		keys := mintIdempotencyKeys(len(changes))
		var rollbackKeys []string
		if rollbackOnError {
			all := mintIdempotencyKeys(2 * len(changes))
			keys = all[:len(changes)]
			rollbackKeys = all[len(changes):]
		}

		var failErr error
		var failIndex int
		for i, change := range changes {
			// computePlan only emits create/update/delete; noops are absent
			// from the list, so a Provider is never invoked for them.
			if err := executeChange(r.Context(), providers[i], change, keys[i]); err != nil {
				// Stop immediately: later changes are skipped. A retryable
				// error exhausted the attempt budget or turned non-retryable;
				// either way the last error text is reported at the change's
				// global index.
				failErr = err
				failIndex = i
				break
			}
			applied = append(applied, change)
			appliedIndex = append(appliedIndex, i)
			applyToState(state, change)
		}

		if failErr != nil && rollbackOnError {
			respondWithRollback(w, r, state, applied, appliedIndex, compensations, compensationProviders, rollbackKeys, failErr, failIndex, summary)
			return
		}

		if failErr != nil {
			// Earlier changes are not rolled back.
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(applyResponse{
				Valid:   false,
				Errors:  []validationError{{Code: codeProviderError, Message: failErr.Error(), Path: fmt.Sprintf("/changes/%d", failIndex)}},
				Applied: &applied,
				Summary: summary,
				State:   renderState(state),
			})
			return
		}

		response := applyResponse{
			Valid:   true,
			Errors:  []validationError{},
			Applied: &applied,
			Summary: summary,
			State:   renderState(state),
		}
		if rollbackOnError {
			// Nothing failed, so nothing was compensated; both arrays are
			// present and empty.
			rolledBack := []planChange{}
			rollbackErrors := []validationError{}
			response.RolledBack = &rolledBack
			response.RollbackErrors = &rollbackErrors
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(response)
	}
}

// compensationFor derives the change that undoes a successfully applied one:
// create is compensated by deleting what was created, delete by recreating
// the before snapshot, and update by swapping before and after.
func compensationFor(change planChange) planChange {
	switch change.Action {
	case "create":
		return planChange{Address: change.Address, Action: "delete", Before: change.After}
	case "delete":
		return planChange{Address: change.Address, Action: "create", After: change.Before}
	default: // "update"
		return planChange{Address: change.Address, Action: "update", Before: change.After, After: change.Before}
	}
}

// respondWithRollback finishes a rollback-enabled run whose forward execution
// failed at failIndex: the successfully applied changes are compensated in
// reverse order of their application, each under the same controlled retry
// policy as a forward change. A compensation that fails finally is recorded
// in rollbackErrors at /rollbacks/{plan index} and its forward change stays
// in effect; compensation then continues with the earlier successes. The
// response keeps the single forward provider_error, lists the compensated
// original changes in compensation order, keeps only the still-effective
// forward changes in applied, and reports the state those residual changes
// leave behind.
func respondWithRollback(w http.ResponseWriter, r *http.Request, state map[string]planResource, applied []planChange, appliedIndex []int, compensations []planChange, compensationProviders []Provider, rollbackKeys []string, failErr error, failIndex int, summary planSummary) {
	rolledBack := []planChange{}
	rollbackErrors := []validationError{}
	compensated := make([]bool, len(applied))
	for j := len(applied) - 1; j >= 0; j-- {
		i := appliedIndex[j]
		if err := executeChange(r.Context(), compensationProviders[i], compensations[i], rollbackKeys[i]); err != nil {
			rollbackErrors = append(rollbackErrors, validationError{
				Code:    codeRollbackError,
				Message: err.Error(),
				Path:    fmt.Sprintf("/rollbacks/%d", i),
			})
			continue
		}
		compensated[j] = true
		rolledBack = append(rolledBack, applied[j])
		// Undoing the change in state is exactly applying its compensation;
		// a failed compensation leaves the forward effect in place.
		applyToState(state, compensations[i])
	}

	// applied keeps only the forward changes still in effect, in their
	// original execution order.
	residual := []planChange{}
	for j, change := range applied {
		if !compensated[j] {
			residual = append(residual, change)
		}
	}

	w.WriteHeader(http.StatusBadGateway)
	_ = json.NewEncoder(w).Encode(applyResponse{
		Valid:          false,
		Errors:         []validationError{{Code: codeProviderError, Message: failErr.Error(), Path: fmt.Sprintf("/changes/%d", failIndex)}},
		Applied:        &residual,
		Summary:        summary,
		State:          renderState(state),
		RolledBack:     &rolledBack,
		RollbackErrors: &rollbackErrors,
	})
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
