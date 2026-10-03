// Package server exposes the frozen public surface of StackWeaver.
//
// The baseline only reports process health. Later work adds the capabilities
// described in README.md; keep the exported surface here backward compatible.
package server

import (
	"encoding/json"
	"net/http"
)

// Version is the baseline release identifier.
const Version = "0.1.0"

type health struct {
	Status  string `json:"status"`
	Service string `json:"service"`
	Version string `json:"version"`
}

// Handler returns the HTTP surface served by the baseline. It injects no
// Provider, so POST /v1/apply succeeds only for empty plans and answers 503
// provider_unavailable when there is anything to execute; every other
// endpoint is unaffected.
func Handler() http.Handler {
	return HandlerWithProvider(nil)
}

// HandlerWithProvider returns the HTTP surface with provider injected as the
// executor for POST /v1/apply. Passing a nil Provider keeps the read-only
// surface fully usable while apply reports provider_unavailable for non-empty
// plans. The service never persists changes itself; provider is the only
// component that executes them. Without a state file the state endpoints
// report state_unavailable.
func HandlerWithProvider(provider Provider) http.Handler {
	return newMux(singleRouter(provider), nil)
}

// HandlerWithProviderAndStateFile returns the full HTTP surface backed by the
// state file at path: GET /v1/state reports the stored revision and state,
// and POST /v1/state/apply plans against the file and commits the evolved
// state atomically under a non-blocking exclusive lock. When provider also
// implements Observer, GET /v1/state/drift compares the stored state against
// the observed remote snapshots without changing anything, and
// POST /v1/state/reconcile converges the remote toward the stored state
// without writing or bumping the revision; without an Observer those
// endpoints report observer_unavailable. The request/response behavior of
// every other endpoint is identical to HandlerWithProvider.
func HandlerWithProviderAndStateFile(provider Provider, path string) http.Handler {
	return newMux(singleRouter(provider), newStateBackend(path))
}

// HandlerWithProviders returns the HTTP surface that dispatches every planned
// change to the Provider registered for its resource type: create and update
// route by the after snapshot's type, delete by the before snapshot's type.
// Entries with an empty type key or a nil Provider are treated as
// unregistered, and one Provider instance may serve several types. After
// validation and planning, all non-noop changes are preflighted; the first
// change whose type has no registered Provider fails the run with 503
// provider_unavailable (path /changes/{index}) before any Provider is called.
// An empty plan succeeds even with an empty registry. The read-only surface
// behaves exactly like HandlerWithProvider.
func HandlerWithProviders(providers map[string]Provider) http.Handler {
	return newMux(multiRouter(providers), nil)
}

// HandlerWithProvidersAndStateFile combines the type-dispatching registry of
// HandlerWithProviders with the state-file backend of
// HandlerWithProviderAndStateFile. Apply runs preflight every planned change
// before executing; drift and reconcile preflight every saved resource in
// ascending address order under the state lock, reporting 503
// provider_unavailable for an unregistered type and 503 observer_unavailable
// when the registered Provider does not implement Observer (path
// /resources/{index} in both cases), without calling Observe or Apply or
// writing state. All other behavior — locking, drift comparison, topological
// execution, error stops, and response shapes — matches the single-Provider
// wiring.
func HandlerWithProvidersAndStateFile(providers map[string]Provider, path string) http.Handler {
	return newMux(multiRouter(providers), newStateBackend(path))
}

func newMux(router providerRouter, backend *stateBackend) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/configurations/validate", handleValidateConfiguration)
	mux.HandleFunc("/v1/configurations/order", handleOrderConfiguration)
	mux.HandleFunc("/v1/plans", handlePlan)
	mux.HandleFunc("/v1/apply", handleApply(router))
	if backend != nil {
		mux.HandleFunc("/v1/state", backend.handleRead)
		mux.HandleFunc("/v1/state/apply", backend.handleApply(router))
		mux.HandleFunc("/v1/state/drift", backend.handleDrift(router))
		mux.HandleFunc("/v1/state/reconcile", backend.handleReconcile(router))
	} else {
		mux.HandleFunc("/v1/state", handleStateUnavailable)
		mux.HandleFunc("/v1/state/apply", handleStateUnavailable)
		mux.HandleFunc("/v1/state/drift", handleStateUnavailable)
		mux.HandleFunc("/v1/state/reconcile", handleStateUnavailable)
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, `{"error":{"code":"method_not_allowed"}}`, http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(health{Status: "ok", Service: "stackweaver", Version: Version})
	})
	return mux
}
