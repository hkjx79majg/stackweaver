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
// component that executes them.
func HandlerWithProvider(provider Provider) http.Handler {
	return newHandler(provider, "")
}

// HandlerWithProviderAndStateFile returns the full HTTP surface backed by a
// state file at path. It serves everything HandlerWithProvider serves and
// additionally GET /v1/state, which reports the file's revision and state,
// and POST /v1/state/apply, which plans a configuration against the file,
// executes the changes through provider, and atomically commits the result
// with the revision incremented by one. A missing file reads as revision 0
// with empty resources. Handlers created without a state file answer the
// state endpoints with 503 state_unavailable.
func HandlerWithProviderAndStateFile(provider Provider, path string) http.Handler {
	return newHandler(provider, path)
}

func newHandler(provider Provider, statePath string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/configurations/validate", handleValidateConfiguration)
	mux.HandleFunc("/v1/configurations/order", handleOrderConfiguration)
	mux.HandleFunc("/v1/plans", handlePlan)
	mux.HandleFunc("/v1/apply", handleApply(provider))
	if statePath != "" {
		mux.HandleFunc("/v1/state", handleGetState(statePath))
		mux.HandleFunc("/v1/state/apply", handleStateApply(provider, statePath))
	} else {
		mux.HandleFunc("/v1/state", handleStateUnavailable)
		mux.HandleFunc("/v1/state/apply", handleStateUnavailable)
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
