package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
)

// Error codes reported by the read-only drift endpoint, alongside the shared
// state codes (state_unavailable, state_locked, state_read_error).
const (
	codeObserverUnavailable   = "observer_unavailable"
	codeObserverError         = "observer_error"
	codeObserverInvalidResult = "observer_invalid_result"
)

// driftEntry is one detected difference between the stored state and the
// observed remote. Status is "changed" when both sides exist but differ, or
// "missing" when the remote resource is gone; Observed is omitted for the
// latter.
type driftEntry struct {
	Address  string    `json:"address"`
	Status   string    `json:"status"`
	Before   *Snapshot `json:"before"`
	Observed *Snapshot `json:"observed,omitempty"`
}

// driftSummary counts the compared resources by outcome.
type driftSummary struct {
	Unchanged int `json:"unchanged"`
	Changed   int `json:"changed"`
	Missing   int `json:"missing"`
}

// driftResponse is the GET /v1/state/drift payload. Failure outcomes carry
// only Valid and Errors and never include partial drifts.
type driftResponse struct {
	Valid    bool              `json:"valid"`
	Errors   []validationError `json:"errors"`
	Revision int64             `json:"revision"`
	Drifts   []driftEntry      `json:"drifts"`
	Summary  driftSummary      `json:"summary"`
}

// observerOf reports provider's Observer capability, if it has one.
func observerOf(provider Provider) Observer {
	observer, ok := provider.(Observer)
	if !ok {
		return nil
	}
	return observer
}

// handleDrift serves GET /v1/state/drift: it compares the stored state
// against the remote snapshots reported by the resolved Observers, address by
// address in ascending Unicode order. The run is strictly read-only: it never
// calls Apply, never writes the state file, and never bumps the revision.
func (b *stateBackend) handleDrift(source providerSource) http.HandlerFunc {
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

		// The Observer capability check the mode runs before any locking or
		// I/O.
		if !source.checkObserverPreLock(w, "drift detection requires the provider to implement Observer") {
			return
		}

		// The same non-blocking exclusive lock guards apply runs and drift
		// detection, so an observation never races a commit.
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

		// Observe in ascending address order; that order indexes errors and
		// sorts the resulting drifts. An empty state never reaches an
		// Observer.
		resources := make([]planResource, len(current.resources))
		copy(resources, current.resources)
		sort.Slice(resources, func(i, j int) bool { return resources[i].address < resources[j].address })

		// Precheck: every resource must route to a Provider implementing
		// Observer before anything is observed. Any failure aborts the run
		// without calling Observe or writing state.
		observers := make([]Observer, len(resources))
		for i, resource := range resources {
			observer, precheckErr := source.observerFor(resource, i)
			if precheckErr != nil {
				writeValidation(w, http.StatusServiceUnavailable, []validationError{*precheckErr})
				return
			}
			observers[i] = observer
		}

		drifts := []driftEntry{}
		var summary driftSummary
		for i, resource := range resources {
			observed, observeErr := observers[i].Observe(r.Context(), resource.address)
			if observeErr != nil {
				// Stop immediately: later resources are not observed and no
				// partial drift list is reported.
				writeValidation(w, http.StatusBadGateway, []validationError{{
					Code:    codeObserverError,
					Message: observeErr.Error(),
					Path:    fmt.Sprintf("/resources/%d", i),
				}})
				return
			}

			before := snapshotOf(resource)
			if observed == nil {
				summary.Missing++
				drifts = append(drifts, driftEntry{Address: resource.address, Status: "missing", Before: &before})
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

			if snapshotsEquivalent(before, normalized) {
				summary.Unchanged++
				continue
			}
			summary.Changed++
			drifts = append(drifts, driftEntry{
				Address:  resource.address,
				Status:   "changed",
				Before:   &before,
				Observed: &normalized,
			})
		}

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(driftResponse{
			Valid:    true,
			Errors:   []validationError{},
			Revision: current.revision,
			Drifts:   drifts,
			Summary:  summary,
		})
	}
}

// normalizeObserved validates one observed snapshot against the Observer
// contract and re-expresses it in the canonical form used for comparison and
// output: dependencies sorted ascending and properties round-tripped through
// JSON so they hold only plain decoded JSON values. A violation is reported
// to the caller as observer_invalid_result.
func normalizeObserved(snapshot *Snapshot) (Snapshot, error) {
	if snapshot.Type == "" {
		return Snapshot{}, fmt.Errorf("observed snapshot must have a non-empty type")
	}
	if snapshot.Properties == nil {
		return Snapshot{}, fmt.Errorf("observed snapshot must have non-nil properties")
	}
	encoded, err := json.Marshal(snapshot.Properties)
	if err != nil {
		return Snapshot{}, fmt.Errorf("observed properties cannot be encoded as JSON: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(encoded))
	dec.UseNumber()
	var decoded any
	if err := dec.Decode(&decoded); err != nil {
		return Snapshot{}, fmt.Errorf("observed properties cannot be encoded as JSON: %v", err)
	}
	properties, ok := decoded.(map[string]any)
	if !ok {
		return Snapshot{}, fmt.Errorf("observed properties must encode as a JSON object")
	}

	seen := make(map[string]bool, len(snapshot.DependsOn))
	dependsOn := make([]string, len(snapshot.DependsOn))
	for i, dep := range snapshot.DependsOn {
		if dep == "" {
			return Snapshot{}, fmt.Errorf("observed dependsOn contains an empty address")
		}
		if seen[dep] {
			return Snapshot{}, fmt.Errorf("observed dependsOn contains duplicate %q", dep)
		}
		seen[dep] = true
		dependsOn[i] = dep
	}
	sort.Strings(dependsOn)

	return Snapshot{Type: snapshot.Type, Properties: properties, DependsOn: dependsOn}, nil
}

// snapshotsEquivalent compares two snapshots with plan semantics: object key
// order and numerically equal spellings are ignored, arrays stay ordered,
// and dependencies compare as sets.
func snapshotsEquivalent(a, b Snapshot) bool {
	return resourceEqual(
		planResource{typ: a.Type, properties: a.Properties, dependsOn: a.DependsOn},
		planResource{typ: b.Type, properties: b.Properties, dependsOn: b.DependsOn},
	)
}
