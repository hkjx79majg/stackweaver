package server

import (
	"context"
	"net/http"
	"testing"
)

// multiPlanBody wraps a configuration and priorState fragment in a plan
// request that declares the resource types a, b, and c.
func multiPlanBody(configResources, stateResources string) string {
	return `{
	  "configuration": {
	    "resourceTypes": [
	      {"name": "a", "schema": {"type": "object", "additionalProperties": true}},
	      {"name": "b", "schema": {"type": "object", "additionalProperties": true}},
	      {"name": "c", "schema": {"type": "object", "additionalProperties": true}}
	    ],
	    "resources": ` + configResources + `
	  },
	  "priorState": {
	    "resources": ` + stateResources + `
	  }
	}`
}

// multiStateBody is the state-apply envelope counterpart of multiPlanBody.
func multiStateBody(configResources string) string {
	return `{
	  "configuration": {
	    "resourceTypes": [
	      {"name": "a", "schema": {"type": "object", "additionalProperties": true}},
	      {"name": "b", "schema": {"type": "object", "additionalProperties": true}},
	      {"name": "c", "schema": {"type": "object", "additionalProperties": true}}
	    ],
	    "resources": ` + configResources + `
	  }
	}`
}

// typedEntry is a configuration resource entry with an explicit type.
func typedEntry(address, typ string) string {
	return `{"address": "` + address + `", "type": "` + typ + `", "properties": {}}`
}

func TestProvidersApplyRoutesByChangeType(t *testing.T) {
	provA := &recordingProvider{}
	provB := &recordingProvider{}
	handler := HandlerWithProviders(map[string]Provider{"a": provA, "b": provB})

	// a1 changes type from b to a (update routed to the target type a),
	// b1 is created (type b), old is deleted (before type b).
	body := multiPlanBody(
		`[`+typedEntry("a1", "a")+`,`+typedEntry("b1", "b")+`]`,
		`[`+stateEntry("a1", "b", `{}`, ``)+`,`+stateEntry("old", "b", `{}`, ``)+`]`)
	res := doApply(t, handler, body, nil)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
	if got := providerCallsSummary(provA.calls); got != "update:a1" {
		t.Fatalf("provider a calls = %q, want update:a1", got)
	}
	if got := providerCallsSummary(provB.calls); got != "delete:old|create:b1" {
		t.Fatalf("provider b calls = %q, want delete:old|create:b1", got)
	}
	// The type-changing update hands both snapshots over unchanged.
	call := provA.calls[0]
	if call.Before == nil || call.Before.Type != "b" || call.After == nil || call.After.Type != "a" {
		t.Fatalf("type-changing update carried before=%+v after=%+v", call.Before, call.After)
	}
}

func TestProvidersApplyPassesRequestContext(t *testing.T) {
	ctx := context.WithValue(context.Background(), struct{ k string }{"k"}, "v")
	provA := &recordingProvider{ctx: ctx}
	handler := HandlerWithProviders(map[string]Provider{"a": provA})
	res := doApply(t, handler, multiPlanBody(`[`+typedEntry("a1", "a")+`]`, `[]`), ctx)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
	if len(provA.calls) != 1 || provA.calls[0].Context != ctx {
		t.Fatalf("provider did not receive the inbound context: %+v", provA.calls)
	}
}

func TestProvidersApplySameInstanceServesMultipleTypes(t *testing.T) {
	shared := &recordingProvider{}
	handler := HandlerWithProviders(map[string]Provider{"a": shared, "b": shared})
	body := multiPlanBody(`[`+typedEntry("a1", "a")+`,`+typedEntry("b1", "b")+`]`, `[]`)
	res := doApply(t, handler, body, nil)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
	if got := providerCallsSummary(shared.calls); got != "create:a1|create:b1" {
		t.Fatalf("shared provider calls = %q, want create:a1|create:b1", got)
	}
}

func TestProvidersApplyPreflightUnregisteredType(t *testing.T) {
	provA := &recordingProvider{}
	handler := HandlerWithProviders(map[string]Provider{"a": provA})
	// create:a1 is routable, create:c1 is not; the unrouted change sits at
	// global index 1 and nothing may be executed.
	body := multiPlanBody(`[`+typedEntry("a1", "a")+`,`+typedEntry("c1", "c")+`]`, `[]`)
	res := doApply(t, handler, body, nil)
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeProviderUnavailable || res.Errors[0].Path != "/changes/1" {
		t.Fatalf("errors = %+v, want provider_unavailable at /changes/1", res.Errors)
	}
	if len(provA.calls) != 0 {
		t.Fatalf("provider called %d times despite preflight failure", len(provA.calls))
	}
	if res.Applied != nil {
		t.Fatalf("applied = %+v, want omitted", res.Applied)
	}
}

func TestProvidersApplyEmptyAndNilEntriesAreUnregistered(t *testing.T) {
	provA := &recordingProvider{}
	handler := HandlerWithProviders(map[string]Provider{"": provA, "a": nil})
	res := doApply(t, handler, multiPlanBody(`[`+typedEntry("a1", "a")+`]`, `[]`), nil)
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeProviderUnavailable || res.Errors[0].Path != "/changes/0" {
		t.Fatalf("errors = %+v, want provider_unavailable at /changes/0", res.Errors)
	}
	if len(provA.calls) != 0 {
		t.Fatalf("provider under empty key called %d times", len(provA.calls))
	}
}

func TestProvidersApplyEmptyPlanSucceedsWithoutRegistry(t *testing.T) {
	res := doApply(t, HandlerWithProviders(nil), multiPlanBody(`[]`, `[]`), nil)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
	if res.Applied == nil || len(*res.Applied) != 0 {
		t.Fatalf("applied = %v, want empty array", res.Applied)
	}
}

func TestProvidersApplyProviderErrorStopsImmediately(t *testing.T) {
	provA := &recordingProvider{}
	provB := &recordingProvider{failAt: 0, failErr: errTestProviders}
	handler := HandlerWithProviders(map[string]Provider{"a": provA, "b": provB})
	body := multiPlanBody(`[`+typedEntry("a1", "a")+`,`+typedEntry("b1", "b")+`]`, `[]`)
	res := doApply(t, handler, body, nil)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeProviderError || res.Errors[0].Path != "/changes/1" {
		t.Fatalf("errors = %+v, want provider_error at /changes/1", res.Errors)
	}
	if res.Applied == nil || len(*res.Applied) != 1 || (*res.Applied)[0].Address != "a1" {
		t.Fatalf("applied = %+v, want only a1", res.Applied)
	}
	if got := providerCallsSummary(provA.calls); got != "create:a1" {
		t.Fatalf("provider a calls = %q, want create:a1", got)
	}
}

var errTestProviders = errorString("boom")

type errorString string

func (e errorString) Error() string { return string(e) }

func TestProvidersStateApplyPreflightKeepsFileAndRevision(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 5, `[`+stateEntry("old", "a", `{}`, ``)+`]`)
	provA := &recordingProvider{}
	handler := HandlerWithProvidersAndStateFile(map[string]Provider{"a": provA}, path)

	// delete:old routes to a (registered), create:c1 does not.
	res := doStateApply(t, handler, multiStateBody(`[`+typedEntry("c1", "c")+`]`))
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeProviderUnavailable || res.Errors[0].Path != "/changes/1" {
		t.Fatalf("errors = %+v, want provider_unavailable at /changes/1", res.Errors)
	}
	if res.Revision == nil || *res.Revision != 5 {
		t.Fatalf("revision = %v, want 5", res.Revision)
	}
	if len(provA.calls) != 0 {
		t.Fatalf("provider called %d times despite preflight failure", len(provA.calls))
	}
	revision, resources := readStateFileRaw(t, path)
	if revision != 5 || len(resources) != 1 || resources[0].Address != "old" {
		t.Fatalf("state file changed: revision=%d resources=%+v", revision, resources)
	}
}

func TestProvidersStateApplyRoutesAndCommits(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 2, `[]`)
	provA := &recordingProvider{}
	provB := &recordingProvider{}
	handler := HandlerWithProvidersAndStateFile(map[string]Provider{"a": provA, "b": provB}, path)

	res := doStateApply(t, handler, multiStateBody(`[`+typedEntry("a1", "a")+`,`+typedEntry("b1", "b")+`]`))
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
	if res.Revision == nil || *res.Revision != 3 {
		t.Fatalf("revision = %v, want 3", res.Revision)
	}
	if got := providerCallsSummary(provA.calls); got != "create:a1" {
		t.Fatalf("provider a calls = %q, want create:a1", got)
	}
	if got := providerCallsSummary(provB.calls); got != "create:b1" {
		t.Fatalf("provider b calls = %q, want create:b1", got)
	}
	revision, resources := readStateFileRaw(t, path)
	if revision != 3 || len(resources) != 2 {
		t.Fatalf("state file: revision=%d resources=%+v", revision, resources)
	}
}

func TestProvidersStateApplyEmptyPlanSucceedsWithoutRegistry(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 4, `[]`)
	handler := HandlerWithProvidersAndStateFile(nil, path)
	res := doStateApply(t, handler, multiStateBody(`[]`))
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
	if res.Revision == nil || *res.Revision != 4 {
		t.Fatalf("revision = %v, want 4", res.Revision)
	}
}

func TestProvidersDriftRoutesObserversBySavedType(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 7, `[
		`+stateEntry("r1", "a", `{"x": 1}`, ``)+`,
		`+stateEntry("r2", "b", `{"y": 2}`, ``)+`
	]`)
	obsA := &driftProvider{snapshots: map[string]*Snapshot{
		"r1": {Type: "a", Properties: map[string]any{"x": 1.0}, DependsOn: []string{}},
	}}
	obsB := &driftProvider{snapshots: map[string]*Snapshot{
		"r2": {Type: "b", Properties: map[string]any{"y": 2.0}, DependsOn: []string{}},
	}}
	handler := HandlerWithProvidersAndStateFile(map[string]Provider{"a": obsA, "b": obsB}, path)

	res := doDriftGet(t, handler)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
	if res.Revision == nil || *res.Revision != 7 {
		t.Fatalf("revision = %v, want 7", res.Revision)
	}
	if res.Summary == nil || res.Summary.Unchanged != 2 || res.Summary.Changed != 0 || res.Summary.Missing != 0 {
		t.Fatalf("summary = %+v, want 2 unchanged", res.Summary)
	}
	if got := providerCallsSummary2(obsA.calls); got != "r1" {
		t.Fatalf("observer a calls = %q, want r1", got)
	}
	if got := providerCallsSummary2(obsB.calls); got != "r2" {
		t.Fatalf("observer b calls = %q, want r2", got)
	}
}

func providerCallsSummary2(calls []string) string {
	out := ""
	for i, c := range calls {
		if i > 0 {
			out += "|"
		}
		out += c
	}
	return out
}

func TestProvidersDriftUnregisteredTypePreflight(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 1, `[
		`+stateEntry("r1", "a", `{}`, ``)+`,
		`+stateEntry("r2", "b", `{}`, ``)+`
	]`)
	obsA := &driftProvider{}
	handler := HandlerWithProvidersAndStateFile(map[string]Provider{"a": obsA}, path)

	res := doDriftGet(t, handler)
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeProviderUnavailable || res.Errors[0].Path != "/resources/1" {
		t.Fatalf("errors = %+v, want provider_unavailable at /resources/1", res.Errors)
	}
	if len(obsA.calls) != 0 {
		t.Fatalf("Observe called %d times despite preflight failure", len(obsA.calls))
	}
}

func TestProvidersDriftObserverUnavailablePreflight(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 1, `[
		`+stateEntry("r1", "a", `{}`, ``)+`,
		`+stateEntry("r2", "b", `{}`, ``)+`
	]`)
	obsA := &driftProvider{}
	handler := HandlerWithProvidersAndStateFile(map[string]Provider{"a": obsA, "b": &recordingProvider{}}, path)

	res := doDriftGet(t, handler)
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeObserverUnavailable || res.Errors[0].Path != "/resources/1" {
		t.Fatalf("errors = %+v, want observer_unavailable at /resources/1", res.Errors)
	}
	if len(obsA.calls) != 0 {
		t.Fatalf("Observe called %d times despite preflight failure", len(obsA.calls))
	}
}

func TestProvidersDriftEmptyStateSucceedsWithoutRegistry(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 2, `[]`)
	res := doDriftGet(t, HandlerWithProvidersAndStateFile(nil, path))
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
	if res.Drifts == nil || len(*res.Drifts) != 0 {
		t.Fatalf("drifts = %+v, want empty", res.Drifts)
	}
}

func TestProvidersReconcileRoutesObserveAndApply(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 3, `[
		`+stateEntry("r1", "a", `{"x": 1}`, ``)+`,
		`+stateEntry("r2", "b", `{"y": 2}`, ``)+`
	]`)
	// r1 is missing remotely (recreate), r2 differs (update).
	obsA := &driftProvider{snapshots: map[string]*Snapshot{}}
	obsB := &driftProvider{snapshots: map[string]*Snapshot{
		"r2": {Type: "b", Properties: map[string]any{"y": 99}, DependsOn: []string{}},
	}}
	handler := HandlerWithProvidersAndStateFile(map[string]Provider{"a": obsA, "b": obsB}, path)

	res := doReconcile(t, handler, `{}`)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
	if res.Applied == nil || len(*res.Applied) != 2 {
		t.Fatalf("applied = %+v, want 2 changes", res.Applied)
	}
	if (*res.Applied)[0].Action != "create" || (*res.Applied)[0].Address != "r1" ||
		(*res.Applied)[1].Action != "update" || (*res.Applied)[1].Address != "r2" {
		t.Fatalf("applied = %+v, want create:r1 then update:r2", *res.Applied)
	}
	if obsA.applies != 1 || obsB.applies != 1 {
		t.Fatalf("applies: a=%d b=%d, want 1 each", obsA.applies, obsB.applies)
	}
	if got := providerCallsSummary2(obsA.calls); got != "r1" {
		t.Fatalf("observer a calls = %q, want r1", got)
	}
	if got := providerCallsSummary2(obsB.calls); got != "r2" {
		t.Fatalf("observer b calls = %q, want r2", got)
	}
	// Reconcile never writes the state file.
	revision, resources := readStateFileRaw(t, path)
	if revision != 3 || len(resources) != 2 {
		t.Fatalf("state file changed: revision=%d resources=%+v", revision, resources)
	}
}

func TestProvidersReconcileUnregisteredTypePreflight(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 3, `[
		`+stateEntry("r1", "a", `{}`, ``)+`,
		`+stateEntry("r2", "b", `{}`, ``)+`
	]`)
	obsA := &driftProvider{}
	handler := HandlerWithProvidersAndStateFile(map[string]Provider{"a": obsA}, path)

	res := doReconcile(t, handler, `{}`)
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeProviderUnavailable || res.Errors[0].Path != "/resources/1" {
		t.Fatalf("errors = %+v, want provider_unavailable at /resources/1", res.Errors)
	}
	if len(obsA.calls) != 0 || obsA.applies != 0 {
		t.Fatalf("provider touched despite preflight failure: calls=%v applies=%d", obsA.calls, obsA.applies)
	}
}

func TestProvidersReconcileObserverUnavailablePreflight(t *testing.T) {
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 3, `[
		`+stateEntry("r1", "a", `{}`, ``)+`,
		`+stateEntry("r2", "b", `{}`, ``)+`
	]`)
	obsA := &driftProvider{}
	handler := HandlerWithProvidersAndStateFile(map[string]Provider{"a": obsA, "b": &recordingProvider{}}, path)

	res := doReconcile(t, handler, `{}`)
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeObserverUnavailable || res.Errors[0].Path != "/resources/1" {
		t.Fatalf("errors = %+v, want observer_unavailable at /resources/1", res.Errors)
	}
	if len(obsA.calls) != 0 || obsA.applies != 0 {
		t.Fatalf("provider touched despite preflight failure: calls=%v applies=%d", obsA.calls, obsA.applies)
	}
}
