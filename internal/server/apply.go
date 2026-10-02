package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
)

// Provider executes a single planned resource change against a concrete
// backend. Implementations receive the request-scoped context and one change
// at a time; a non-nil error fails the whole application at that change.
//
// The service never persists anything itself: the Provider is the only side
// effect. A nil Provider keeps every other endpoint working and makes
// POST /v1/apply report provider_unavailable when there is work to do.
type Provider interface {
	Apply(ctx context.Context, req ChangeRequest) error
}

// Snapshot is one side of a change: Before for the prior state, After for the
// configuration.
type Snapshot struct {
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties"`
	DependsOn  []string       `json:"dependsOn"`
}

// ChangeRequest is a single ordered change handed to a Provider. Create
// carries only After, delete only Before, update carries both.
type ChangeRequest struct {
	Action  string    `json:"action"`
	Address string    `json:"address"`
	Before  *Snapshot `json:"before,omitempty"`
	After   *Snapshot `json:"after,omitempty"`
}

// applyResponse is the POST /v1/apply body. Applied, Summary, and State are
// omitted (via nil pointers) from responses that do not define them, and
// always present — including as empty arrays/objects — otherwise.
type applyResponse struct {
	Valid   bool              `json:"valid"`
	Errors  []validationError `json:"errors"`
	Applied *[]planChange     `json:"applied,omitempty"`
	Summary *planSummary      `json:"summary,omitempty"`
	State   *applyState       `json:"state,omitempty"`
}

type applyState struct {
	Resources []map[string]any `json:"resources"`
}

// handleApply executes the same ordered plan reported by POST /v1/plans,
// invoking the Provider once per non-noop change. It never persists state
// itself and never rolls back after a failure.
func handleApply(provider Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{"code": "method_not_allowed"},
			})
			return
		}

		prepared, ok := preparePlan(w, r)
		if !ok {
			return
		}
		changes := prepared.changes

		// An empty plan has nothing to execute and succeeds even without a
		// Provider; only real work requires one. The plan is known at this
		// point, so its summary is reported, but there is no application to
		// describe: applied and state are deliberately absent.
		if len(changes) > 0 && provider == nil {
			summary := prepared.summary
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(applyResponse{
				Valid: false,
				Errors: []validationError{{
					Code:    "provider_unavailable",
					Message: "no provider is configured to execute changes",
					Path:    "",
				}},
				Summary: &summary,
			})
			return
		}

		// State evolves from the prior state, keyed by address. Untouched
		// resources keep the exact entries submitted in priorState.
		state := prepared.prior
		applied := make([]planChange, 0, len(changes))
		for i, change := range changes {
			req := ChangeRequest{
				Action:  change.Action,
				Address: change.Address,
				Before:  snapshotPtr(change.Before),
				After:   snapshotPtr(change.After),
			}
			if err := provider.Apply(r.Context(), req); err != nil {
				// Stop immediately: later changes are not attempted and
				// successful changes are not rolled back.
				summary := prepared.summary
				w.WriteHeader(http.StatusBadGateway)
				_ = json.NewEncoder(w).Encode(applyResponse{
					Valid:   false,
					Errors:  []validationError{{Code: "provider_error", Message: err.Error(), Path: fmt.Sprintf("/changes/%d", i)}},
					Applied: &applied,
					Summary: &summary,
					State:   sortedState(state),
				})
				return
			}
			applyStateTransition(state, change)
			applied = append(applied, change)
		}

		summary := prepared.summary
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(applyResponse{
			Valid:   true,
			Errors:  []validationError{},
			Applied: &applied,
			Summary: &summary,
			State:   sortedState(state),
		})
	}
}

// snapshotPtr converts an internal plan snapshot to its exported form.
func snapshotPtr(s *planSnapshot) *Snapshot {
	if s == nil {
		return nil
	}
	return &Snapshot{Type: s.Type, Properties: s.Properties, DependsOn: s.DependsOn}
}

// applyStateTransition folds one successful change into the evolving state:
// create/update adopt the after snapshot, delete removes the resource.
func applyStateTransition(state map[string]map[string]any, change planChange) {
	switch change.Action {
	case "delete":
		delete(state, change.Address)
	case "create", "update":
		after := change.After
		deps := make([]any, len(after.DependsOn))
		for i, dep := range after.DependsOn {
			deps[i] = dep
		}
		state[change.Address] = map[string]any{
			"address":    change.Address,
			"type":       after.Type,
			"properties": after.Properties,
			"dependsOn":  deps,
		}
	}
}

// sortedState renders the evolving state as its response envelope, with
// resources ordered by address in Unicode code point order (UTF-8 byte order
// equals code point order).
func sortedState(entries map[string]map[string]any) *applyState {
	addresses := make([]string, 0, len(entries))
	for address := range entries {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	resources := make([]map[string]any, 0, len(entries))
	for _, address := range addresses {
		resources = append(resources, entries[address])
	}
	return &applyState{Resources: resources}
}
