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

// Handler returns the HTTP surface served by the baseline. POST /v1/apply is
// present but reports provider_unavailable whenever a plan has work to do;
// use HandlerWithProvider to inject a Provider that executes changes.
func Handler() http.Handler {
	return HandlerWithProvider(nil)
}

// HandlerWithProvider returns the HTTP surface with the given Provider
// executing POST /v1/apply. A nil Provider keeps every other endpoint fully
// available and leaves apply executable only for empty plans.
func HandlerWithProvider(provider Provider) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/configurations/validate", handleValidateConfiguration)
	mux.HandleFunc("/v1/configurations/order", handleOrderConfiguration)
	mux.HandleFunc("/v1/plans", handlePlan)
	mux.HandleFunc("/v1/apply", handleApply(provider))
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
