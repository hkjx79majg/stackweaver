// Package server exposes the frozen public surface of StackWeaver.
//
// The baseline reports process health and validates configuration documents
// without persisting anything. Later work adds the capabilities described in
// README.md; keep the exported surface here backward compatible.
package server

import (
	"encoding/json"
	"io"
	"net/http"
)

// Version is the baseline release identifier.
const Version = "0.1.0"

type health struct {
	Status  string `json:"status"`
	Service string `json:"service"`
	Version string `json:"version"`
}

type errorBody struct {
	Error errorPayload `json:"error"`
}

type errorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

// Handler returns the HTTP surface served by the service.
func Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: errorPayload{Code: "method_not_allowed"}})
			return
		}
		writeJSON(w, http.StatusOK, health{Status: "ok", Service: "stackweaver", Version: Version})
	})

	mux.HandleFunc("/v1/configurations/validate", handleValidate)

	return mux
}

func handleValidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: errorPayload{Code: "method_not_allowed"}})
		return
	}

	body := readBody(r)
	errs, parseable := validateConfiguration(body)
	if !parseable {
		writeJSON(w, http.StatusBadRequest, errorBody{
			Error: errorPayload{Code: "invalid_json", Message: "request body must be a single JSON document"},
		})
		return
	}

	resp := validationResponse{Valid: len(errs) == 0, Errors: errs}
	if resp.Errors == nil {
		resp.Errors = []validationError{}
	}
	status := http.StatusOK
	if !resp.Valid {
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, resp)
}

func readBody(r *http.Request) []byte {
	if r.Body == nil {
		return nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil
	}
	return body
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
