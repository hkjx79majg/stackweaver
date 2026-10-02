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
}

// handleApply validates and plans a request exactly like POST /v1/plans, then
// hands the ordered non-noop changes to provider one at a time. It returns a
// handler so the injected Provider is captured once at wiring time.
func handleApply(provider Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.Method != http.MethodPost {
			rejectMethodNotAllowed(w)
			return
		}

		changes, summary, priorResources, ok := preparePlan(w, r)
		if !ok {
			// preparePlan already wrote the 400/422 response; a Provider is
			// never called for an invalid request.
			return
		}

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

		// State starts from priorState and is mutated only as changes
		// succeed; the service holds it for this request alone.
		state := seedState(priorResources)
		applied := []planChange{}

		for i, change := range changes {
			// computePlan only emits create/update/delete; noops are absent
			// from the list, so a Provider is never invoked for them.
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
				w.WriteHeader(http.StatusBadGateway)
				_ = json.NewEncoder(w).Encode(applyResponse{
					Valid:   false,
					Errors:  []validationError{{Code: codeProviderError, Message: err.Error(), Path: fmt.Sprintf("/changes/%d", i)}},
					Applied: &applied,
					Summary: summary,
					State:   renderState(state),
				})
				return
			}
			applied = append(applied, change)
			applyToState(state, change)
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(applyResponse{
			Valid:   true,
			Errors:  []validationError{},
			Applied: &applied,
			Summary: summary,
			State:   renderState(state),
		})
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
