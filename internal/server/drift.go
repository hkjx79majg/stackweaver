package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
)

// Error codes reported by GET /v1/state/drift: the read-only comparison of
// the state file's managed resources against the Observer's live snapshots.
const (
	codeObserverUnavailable   = "observer_unavailable"
	codeObserverError         = "observer_error"
	codeObserverInvalidResult = "observer_invalid_result"
)

// driftEntry is one diverging resource: changed carries both sides, missing
// only the recorded before. Unchanged resources never appear in the list.
type driftEntry struct {
	Address  string    `json:"address"`
	Status   string    `json:"status"`
	Before   *Snapshot `json:"before"`
	Observed *Snapshot `json:"observed,omitempty"`
}

type driftSummary struct {
	Unchanged int `json:"unchanged"`
	Changed   int `json:"changed"`
	Missing   int `json:"missing"`
}

// driftResponse is the GET /v1/state/drift payload. Revision, Drifts and
// Summary are set only on success; failure responses carry valid=false and
// the single error, never partial drifts.
type driftResponse struct {
	Valid    bool              `json:"valid"`
	Errors   []validationError `json:"errors"`
	Revision *int64            `json:"revision,omitempty"`
	Drifts   *[]driftEntry     `json:"drifts,omitempty"`
	Summary  *driftSummary     `json:"summary,omitempty"`
}

// handleDrift serves GET /v1/state/drift: under the state file's
// non-blocking lock it reads the current revision, then observes every
// managed resource in address order through the Provider's Observer
// capability and reports the resources whose live view diverges from the
// recorded state. The run is read-only: it never calls Apply, never writes
// the state file, and never bumps the revision.
func (b *stateBackend) handleDrift(provider Provider) http.HandlerFunc {
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

		// The Observer capability is required before anything else happens;
		// in particular the lock is never taken without it.
		observer, isObserver := provider.(Observer)
		if !isObserver || observer == nil {
			writeDriftError(w, http.StatusServiceUnavailable, codeObserverUnavailable,
				"drift detection requires the provider to implement Observer, but it does not", "")
			return
		}

		unlock, acquired := tryLockState(b.path)
		if !acquired {
			writeDriftError(w, http.StatusConflict, codeStateLocked,
				"state file is locked by another apply run", "")
			return
		}
		defer unlock()

		current, err := readStateFile(b.path)
		if err != nil {
			writeDriftError(w, http.StatusInternalServerError, codeStateReadError,
				"state file cannot be read or does not hold a valid state document", "")
			return
		}

		// Resources are observed in ascending address order (Unicode code
		// points, which matches Go's byte-wise string comparison for UTF-8);
		// the same order indexes the /resources/{i} error paths.
		byAddress := make(map[string]planResource, len(current.resources))
		addresses := make([]string, 0, len(current.resources))
		for _, resource := range current.resources {
			byAddress[resource.address] = resource
			addresses = append(addresses, resource.address)
		}
		sort.Strings(addresses)

		drifts := []driftEntry{}
		summary := driftSummary{}
		for i, address := range addresses {
			stored := byAddress[address]
			observed, err := observer.Observe(r.Context(), address)
			if err != nil {
				// Stop immediately: later resources are not observed and no
				// partial drift list is reported.
				writeDriftError(w, http.StatusBadGateway, codeObserverError,
					err.Error(), fmt.Sprintf("/resources/%d", i))
				return
			}

			before := &Snapshot{
				Type:       stored.typ,
				Properties: stored.properties,
				DependsOn:  sortedStrings(stored.dependsOn),
			}
			if observed == nil {
				summary.Missing++
				drifts = append(drifts, driftEntry{Address: address, Status: "missing", Before: before})
				continue
			}

			normalized, invalid := checkObservedSnapshot(observed)
			if invalid != "" {
				writeDriftError(w, http.StatusBadGateway, codeObserverInvalidResult,
					invalid, fmt.Sprintf("/resources/%d", i))
				return
			}

			if observedMatchesState(stored, observed, normalized) {
				summary.Unchanged++
				continue
			}
			summary.Changed++
			drifts = append(drifts, driftEntry{
				Address: address,
				Status:  "changed",
				Before:  before,
				Observed: &Snapshot{
					Type:       observed.Type,
					Properties: observed.Properties,
					DependsOn:  sortedStrings(observed.DependsOn),
				},
			})
		}

		revision := current.revision
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(driftResponse{
			Valid:    true,
			Errors:   []validationError{},
			Revision: &revision,
			Drifts:   &drifts,
			Summary:  &summary,
		})
	}
}

// writeDriftError writes a drift failure response: valid=false with exactly
// one error and no revision, drifts, or summary.
func writeDriftError(w http.ResponseWriter, status int, code, message, path string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(driftResponse{
		Valid:  false,
		Errors: []validationError{{Code: code, Message: message, Path: path}},
	})
}

// checkObservedSnapshot validates one non-nil Observer result and returns
// its properties normalized to the decoded-JSON shape (json.Number for
// numbers) so they compare equal to state-file properties regardless of Go
// number types. A non-empty message marks the result invalid.
func checkObservedSnapshot(observed *Snapshot) (normalized map[string]any, invalid string) {
	if observed.Type == "" {
		return nil, "observed snapshot has an empty type"
	}
	if observed.Properties == nil {
		return nil, "observed snapshot has no properties"
	}
	seen := make(map[string]bool, len(observed.DependsOn))
	for _, dep := range observed.DependsOn {
		if dep == "" {
			return nil, "observed snapshot has an empty dependency"
		}
		if seen[dep] {
			return nil, fmt.Sprintf("observed snapshot lists dependency %q twice", dep)
		}
		seen[dep] = true
	}
	data, err := json.Marshal(observed.Properties)
	if err != nil {
		return nil, "observed snapshot properties cannot be encoded as JSON"
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var props any
	if err := dec.Decode(&props); err != nil {
		return nil, "observed snapshot properties cannot be encoded as JSON"
	}
	normalized, ok := props.(map[string]any)
	if !ok {
		return nil, "observed snapshot properties cannot be encoded as JSON"
	}
	return normalized, ""
}

// observedMatchesState reports whether an observed snapshot is equivalent to
// the stored resource under plan semantics: same type, deeply equal
// properties (object key order and numerically equal number spellings are
// insignificant, arrays stay ordered), and equal dependency sets.
func observedMatchesState(stored planResource, observed *Snapshot, normalized map[string]any) bool {
	if stored.typ != observed.Type {
		return false
	}
	if !jsonValueEqual(stored.properties, normalized) {
		return false
	}
	return stringSetEqual(stored.dependsOn, observed.DependsOn)
}

// sortedStrings returns a copy of values in ascending order. The copy is
// never nil, so empty dependency lists serialize as [].
func sortedStrings(values []string) []string {
	out := make([]string, len(values))
	copy(out, values)
	sort.Strings(out)
	return out
}
