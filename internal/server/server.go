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
	return newMux(singleSource{provider: provider}, nil)
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
	return newMux(singleSource{provider: provider}, newStateBackend(path))
}

// HandlerWithProviders returns the HTTP surface dispatching each planned
// change to the Provider registered for its resource type: creates and
// updates route by the after snapshot's type (a type-changing update goes to
// the target type's Provider), deletes by the before snapshot's type. Map
// entries with an empty type key or a nil Provider count as unregistered, and
// one instance may serve several types. Before anything executes, every
// non-noop change is prechecked; a change whose type has no registered
// Provider fails the whole run with 503 provider_unavailable indexed into the
// plan, without calling any Provider. An empty plan succeeds with an empty
// registry. Every other endpoint behaves exactly like HandlerWithProvider.
func HandlerWithProviders(providers map[string]Provider) http.Handler {
	return newMux(newMultiSource(providers), nil)
}

// HandlerWithProvidersAndStateFile combines the type-dispatching registry of
// HandlerWithProviders with the state-file backend of
// HandlerWithProviderAndStateFile. GET /v1/state/drift and
// POST /v1/state/reconcile resolve each saved resource's Provider by its
// stored type and require that instance to implement Observer: under the
// state lock, after the revision is read, all resources are prechecked in
// ascending address order — a type without a registered Provider fails with
// 503 provider_unavailable and one whose Provider is no Observer fails with
// 503 observer_unavailable, both indexed into the address-sorted resource
// list and neither calling Observe or Apply nor writing state. Resources are
// never regrouped by Provider; observation, comparison, and execution keep
// the single-Provider orders and response shapes.
func HandlerWithProvidersAndStateFile(providers map[string]Provider, path string) http.Handler {
	return newMux(newMultiSource(providers), newStateBackend(path))
}

func newMux(source providerSource, backend *stateBackend) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/configurations/validate", handleValidateConfiguration)
	mux.HandleFunc("/v1/configurations/order", handleOrderConfiguration)
	mux.HandleFunc("/v1/plans", handlePlan)
	mux.HandleFunc("/v1/apply", handleApply(source))
	if backend != nil {
		mux.HandleFunc("/v1/state", backend.handleRead)
		mux.HandleFunc("/v1/state/apply", backend.handleApply(source))
		mux.HandleFunc("/v1/state/drift", backend.handleDrift(source))
		mux.HandleFunc("/v1/state/reconcile", backend.handleReconcile(source))
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
