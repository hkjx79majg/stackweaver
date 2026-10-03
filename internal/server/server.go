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
	return newMux(provider, nil)
}

// HandlerWithProviderAndStateFile returns the full HTTP surface backed by the
// state file at path: GET /v1/state reports the stored revision and state,
// and POST /v1/state/apply plans against the file and commits the evolved
// state atomically under a non-blocking exclusive lock. When provider also
// implements Observer, GET /v1/state/drift compares the stored state against
// the observed remote snapshots without changing anything; without an
// Observer that endpoint reports observer_unavailable. The request/response
// behavior of every other endpoint is identical to HandlerWithProvider.
func HandlerWithProviderAndStateFile(provider Provider, path string) http.Handler {
	return newMux(provider, newStateBackend(path))
}

func newMux(provider Provider, backend *stateBackend) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/configurations/validate", handleValidateConfiguration)
	mux.HandleFunc("/v1/configurations/order", handleOrderConfiguration)
	mux.HandleFunc("/v1/plans", handlePlan)
	mux.HandleFunc("/v1/apply", handleApply(provider))
	if backend != nil {
		mux.HandleFunc("/v1/state", backend.handleRead)
		mux.HandleFunc("/v1/state/apply", backend.handleApply(provider))
		mux.HandleFunc("/v1/state/drift", backend.handleDrift(observerOf(provider)))
	} else {
		mux.HandleFunc("/v1/state", handleStateUnavailable)
		mux.HandleFunc("/v1/state/apply", handleStateUnavailable)
		mux.HandleFunc("/v1/state/drift", handleStateUnavailable)
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
