package server

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// multiPlanBody wraps configuration and priorState fragments in a plan
// request declaring the two resource types a and b.
func multiPlanBody(configResources, stateResources string) string {
	return `{
	  "configuration": {
	    "resourceTypes": [
	      {"name": "a", "schema": {"type": "object", "additionalProperties": true}},
	      {"name": "b", "schema": {"type": "object", "additionalProperties": true}}
	    ],
	    "resources": ` + configResources + `
	  },
	  "priorState": {
	    "resources": ` + stateResources + `
	  }
	}`
}

// multiStateConfigBody builds a state-apply envelope declaring types a and b.
func multiStateConfigBody(configResources string) string {
	return `{
	  "configuration": {
	    "resourceTypes": [
	      {"name": "a", "schema": {"type": "object", "additionalProperties": true}},
	      {"name": "b", "schema": {"type": "object", "additionalProperties": true}}
	    ],
	    "resources": ` + configResources + `
	  }
	}`
}

// typedEntry is a configuration resource entry with an explicit type.
func typedEntry(address, typ, dependsOn string) string {
	entry := `{"address": "` + address + `", "type": "` + typ + `", "properties": {}`
	if dependsOn != "" {
		entry += `, "dependsOn": ` + dependsOn
	}
	return entry + `}`
}

func TestMultiApplyRoutesByType(t *testing.T) {
	pa := &recordingProvider{}
	pb := &recordingProvider{}
	handler := HandlerWithProviders(map[string]Provider{"a": pa, "b": pb})

	// u1 (create, type a), u2 (create, type b), u3 (update a -> b),
	// plus deletes of old-a (type a) and old-b (type b).
	body := multiPlanBody(
		`[`+typedEntry("u1", "a", `[]`)+`,`+typedEntry("u2", "b", `["u1"]`)+`,`+typedEntry("u3", "b", `[]`)+`]`,
		`[`+stateEntry("u3", "a", `{}`, `[]`)+`,`+stateEntry("old-a", "a", `{}`, `[]`)+`,`+stateEntry("old-b", "b", `{}`, `[]`)+`]`)
	res := doApply(t, handler, body, nil)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}

	// Deletes run first in reverse state order, then creates/updates in
	// configuration topological order; each change lands on its type's
	// Provider: old-a is type a, old-b is type b.
	if got := providerCallsSummary(pa.calls); got != "delete:old-a|create:u1" {
		t.Fatalf("provider a calls = %q", got)
	}
	if got := providerCallsSummary(pb.calls); got != "delete:old-b|create:u2|update:u3" {
		t.Fatalf("provider b calls = %q", got)
	}

	// The type-changing update went to the target type's Provider with both
	// snapshots intact.
	update := pb.calls[2]
	if update.Before == nil || update.Before.Type != "a" || update.After == nil || update.After.Type != "b" {
		t.Fatalf("type-changing update handed over as %+v", update)
	}
	// Deletes routed by the before snapshot's type.
	if pb.calls[0].Before == nil || pb.calls[0].Before.Type != "b" || pb.calls[0].After != nil {
		t.Fatalf("delete handed over as %+v", pb.calls[0])
	}
}

func TestMultiApplySameInstanceServesManyTypes(t *testing.T) {
	shared := &recordingProvider{}
	handler := HandlerWithProviders(map[string]Provider{"a": shared, "b": shared})
	body := multiPlanBody(
		`[`+typedEntry("u1", "a", `[]`)+`,`+typedEntry("u2", "b", `[]`)+`]`,
		`[]`)
	res := doApply(t, handler, body, nil)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
	if got := providerCallsSummary(shared.calls); got != "create:u1|create:u2" {
		t.Fatalf("shared provider calls = %q", got)
	}
}

func TestMultiApplyEmptyPlanSucceedsWithoutRegistry(t *testing.T) {
	for name, handler := range map[string]http.Handler{
		"nil registry":   HandlerWithProviders(nil),
		"empty registry": HandlerWithProviders(map[string]Provider{}),
	} {
		res := doApply(t, handler, planBody(`[]`, `[]`), nil)
		if res.status != http.StatusOK || !res.Valid || len(res.Errors) != 0 {
			t.Fatalf("%s: got status=%d valid=%v errors=%v", name, res.status, res.Valid, res.Errors)
		}
		if res.Applied == nil || len(*res.Applied) != 0 {
			t.Fatalf("%s: applied = %v, want empty array", name, res.Applied)
		}
	}
}

func TestMultiApplyUnregisteredTypeFailsPrecheck(t *testing.T) {
	pa := &recordingProvider{}
	handler := HandlerWithProviders(map[string]Provider{"a": pa})
	// u1 (type a) is routable, u2 (type b) is not: the run must abort before
	// u1 reaches its Provider, reporting u2's global plan index.
	body := multiPlanBody(
		`[`+typedEntry("u1", "a", `[]`)+`,`+typedEntry("u2", "b", `[]`)+`]`,
		`[]`)
	res := doApply(t, handler, body, nil)
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.status)
	}
	if res.Valid || len(res.Errors) != 1 || res.Errors[0].Code != codeProviderUnavailable {
		t.Fatalf("unexpected body %+v", res)
	}
	if res.Errors[0].Path != "/changes/1" {
		t.Fatalf("error path = %q, want /changes/1", res.Errors[0].Path)
	}
	if len(pa.calls) != 0 {
		t.Fatalf("provider called %d times despite failed precheck", len(pa.calls))
	}
}

func TestMultiApplyEmptyKeyAndNilValueAreUnregistered(t *testing.T) {
	pa := &recordingProvider{}
	handler := HandlerWithProviders(map[string]Provider{"": pa, "b": nil})
	body := multiPlanBody(`[`+typedEntry("u2", "b", `[]`)+`]`, `[]`)
	res := doApply(t, handler, body, nil)
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeProviderUnavailable || res.Errors[0].Path != "/changes/0" {
		t.Fatalf("unexpected errors %+v", res.Errors)
	}
	if len(pa.calls) != 0 {
		t.Fatalf("empty-key provider called")
	}
}

func TestMultiApplyProviderErrorStopsRun(t *testing.T) {
	pa := &recordingProvider{}
	pb := &recordingProvider{failAt: 0, failErr: errors.New("boom")}
	handler := HandlerWithProviders(map[string]Provider{"a": pa, "b": pb})
	body := multiPlanBody(
		`[`+typedEntry("u1", "b", `[]`)+`,`+typedEntry("u2", "a", `["u1"]`)+`]`,
		`[]`)
	res := doApply(t, handler, body, nil)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeProviderError || res.Errors[0].Path != "/changes/0" {
		t.Fatalf("unexpected errors %+v", res.Errors)
	}
	if res.Applied == nil || len(*res.Applied) != 0 {
		t.Fatalf("applied = %v, want empty", res.Applied)
	}
	if len(pa.calls) != 0 {
		t.Fatalf("later provider called after failure")
	}
}

func TestMultiStateApplyRoutesAndCommits(t *testing.T) {
	pa := &recordingProvider{}
	pb := &recordingProvider{}
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 7, `[`+stateEntry("old", "b", `{}`, `[]`)+`]`)
	handler := HandlerWithProvidersAndStateFile(map[string]Provider{"a": pa, "b": pb}, path)

	res := doStateApply(t, handler, multiStateConfigBody(`[`+typedEntry("u1", "a", `[]`)+`]`))
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
	if res.Revision == nil || *res.Revision != 8 {
		t.Fatalf("revision = %v, want 8", res.Revision)
	}
	if got := providerCallsSummary(pb.calls); got != "delete:old" {
		t.Fatalf("provider b calls = %q", got)
	}
	if got := providerCallsSummary(pa.calls); got != "create:u1" {
		t.Fatalf("provider a calls = %q", got)
	}
	revision, resources := readStateFileRaw(t, path)
	if revision != 8 || len(resources) != 1 || resources[0].Address != "u1" || resources[0].Type != "a" {
		t.Fatalf("committed state = revision %d resources %+v", revision, resources)
	}
}

func TestMultiStateApplyUnregisteredTypeLeavesFileUntouched(t *testing.T) {
	pa := &recordingProvider{}
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 3, `[]`)
	handler := HandlerWithProvidersAndStateFile(map[string]Provider{"a": pa}, path)

	res := doStateApply(t, handler, multiStateConfigBody(
		`[`+typedEntry("u1", "a", `[]`)+`,`+typedEntry("u2", "b", `[]`)+`]`))
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeProviderUnavailable || res.Errors[0].Path != "/changes/1" {
		t.Fatalf("unexpected errors %+v", res.Errors)
	}
	if res.Revision == nil || *res.Revision != 3 {
		t.Fatalf("revision = %v, want unchanged 3", res.Revision)
	}
	if len(pa.calls) != 0 {
		t.Fatalf("provider called despite failed precheck")
	}
	revision, resources := readStateFileRaw(t, path)
	if revision != 3 || len(resources) != 0 {
		t.Fatalf("state file changed to revision %d resources %+v", revision, resources)
	}
}

func TestMultiDriftRoutesObserversByType(t *testing.T) {
	pa := &driftProvider{snapshots: map[string]*Snapshot{
		"r1": {Type: "a", Properties: map[string]any{}, DependsOn: []string{}},
	}}
	pb := &driftProvider{snapshots: map[string]*Snapshot{
		"r2": {Type: "b", Properties: map[string]any{"x": "1"}, DependsOn: []string{}},
	}}
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 5, `[`+
		stateEntry("r1", "a", `{}`, `[]`)+`,`+
		stateEntry("r2", "b", `{}`, `[]`)+`,`+
		stateEntry("r3", "b", `{}`, `[]`)+`]`)
	handler := HandlerWithProvidersAndStateFile(map[string]Provider{"a": pa, "b": pb}, path)

	res := doDriftGet(t, handler)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
	// Each resource was observed by its own type's Observer, in ascending
	// address order within each instance.
	if strings.Join(pa.calls, "|") != "r1" {
		t.Fatalf("observer a calls = %v", pa.calls)
	}
	if strings.Join(pb.calls, "|") != "r2|r3" {
		t.Fatalf("observer b calls = %v", pb.calls)
	}
	if res.Summary == nil || res.Summary.Unchanged != 1 || res.Summary.Changed != 1 || res.Summary.Missing != 1 {
		t.Fatalf("summary = %+v", res.Summary)
	}
	if res.Drifts == nil || len(*res.Drifts) != 2 {
		t.Fatalf("drifts = %+v", res.Drifts)
	}
	if pa.applies != 0 || pb.applies != 0 {
		t.Fatalf("drift called Apply")
	}
	if revision, _ := readStateFileRaw(t, path); revision != 5 {
		t.Fatalf("drift rewrote the state file to revision %d", revision)
	}
}

func TestMultiDriftUnregisteredTypeFailsPrecheck(t *testing.T) {
	pa := &driftProvider{}
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 2, `[`+
		stateEntry("r1", "a", `{}`, `[]`)+`,`+
		stateEntry("r2", "b", `{}`, `[]`)+`]`)
	handler := HandlerWithProvidersAndStateFile(map[string]Provider{"a": pa}, path)

	res := doDriftGet(t, handler)
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeProviderUnavailable || res.Errors[0].Path != "/resources/1" {
		t.Fatalf("unexpected errors %+v", res.Errors)
	}
	if len(pa.calls) != 0 {
		t.Fatalf("Observe called despite failed precheck")
	}
}

func TestMultiDriftNonObserverFailsPrecheck(t *testing.T) {
	// recordingProvider is a Provider without the Observer capability.
	pa := &driftProvider{}
	plain := &recordingProvider{}
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 2, `[`+
		stateEntry("r1", "b", `{}`, `[]`)+`,`+
		stateEntry("r2", "a", `{}`, `[]`)+`]`)
	handler := HandlerWithProvidersAndStateFile(map[string]Provider{"a": pa, "b": plain}, path)

	res := doDriftGet(t, handler)
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeObserverUnavailable || res.Errors[0].Path != "/resources/0" {
		t.Fatalf("unexpected errors %+v", res.Errors)
	}
	if len(pa.calls) != 0 {
		t.Fatalf("Observe called despite failed precheck")
	}
}

func TestMultiDriftEmptyStateSucceedsWithEmptyRegistry(t *testing.T) {
	path := stateFilePath(t)
	handler := HandlerWithProvidersAndStateFile(map[string]Provider{}, path)
	res := doDriftGet(t, handler)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
	if res.Drifts == nil || len(*res.Drifts) != 0 {
		t.Fatalf("drifts = %+v, want empty", res.Drifts)
	}
}

func TestMultiReconcileRoutesByType(t *testing.T) {
	pa := &reconcileProvider{snapshots: map[string]*Snapshot{}}
	pb := &reconcileProvider{snapshots: map[string]*Snapshot{
		"r2": {Type: "b", Properties: map[string]any{}, DependsOn: []string{}},
		"r3": {Type: "b", Properties: map[string]any{"y": "2"}, DependsOn: []string{}},
	}}
	path := stateFilePath(t)
	// r1 (type a) is missing remotely and recreated; r2 (type b) matches and
	// is a noop; r3 (type b) drifts and is updated. r3 depends on r1, so the
	// create executes before the update across Providers.
	writeDriftStateFile(t, path, 4, `[`+
		stateEntry("r1", "a", `{}`, `[]`)+`,`+
		stateEntry("r2", "b", `{}`, `[]`)+`,`+
		stateEntry("r3", "b", `{}`, `["r1"]`)+`]`)
	handler := HandlerWithProvidersAndStateFile(map[string]Provider{"a": pa, "b": pb}, path)

	res := doReconcile(t, handler, `{}`)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
	if strings.Join(pa.observes, "|") != "r1" {
		t.Fatalf("observer a calls = %v", pa.observes)
	}
	if strings.Join(pb.observes, "|") != "r2|r3" {
		t.Fatalf("observer b calls = %v", pb.observes)
	}
	if got := providerCallsSummary(pa.applies); got != "create:r1" {
		t.Fatalf("provider a applies = %q", got)
	}
	if got := providerCallsSummary(pb.applies); got != "update:r3" {
		t.Fatalf("provider b applies = %q", got)
	}
	if res.Revision == nil || *res.Revision != 4 {
		t.Fatalf("revision = %v, want unchanged 4", res.Revision)
	}
	if revision, _ := readStateFileRaw(t, path); revision != 4 {
		t.Fatalf("reconcile rewrote the state file to revision %d", revision)
	}
}

func TestMultiReconcileUnregisteredTypeFailsPrecheck(t *testing.T) {
	pa := &reconcileProvider{snapshots: map[string]*Snapshot{}}
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 2, `[`+
		stateEntry("r1", "a", `{}`, `[]`)+`,`+
		stateEntry("r2", "b", `{}`, `[]`)+`]`)
	handler := HandlerWithProvidersAndStateFile(map[string]Provider{"a": pa}, path)

	res := doReconcile(t, handler, `{}`)
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeProviderUnavailable || res.Errors[0].Path != "/resources/1" {
		t.Fatalf("unexpected errors %+v", res.Errors)
	}
	if len(pa.observes) != 0 || len(pa.applies) != 0 {
		t.Fatalf("provider called despite failed precheck")
	}
}

func TestMultiReconcileNonObserverFailsPrecheck(t *testing.T) {
	pa := &reconcileProvider{snapshots: map[string]*Snapshot{}}
	plain := &recordingProvider{}
	path := stateFilePath(t)
	writeDriftStateFile(t, path, 2, `[`+
		stateEntry("r1", "a", `{}`, `[]`)+`,`+
		stateEntry("r2", "b", `{}`, `[]`)+`]`)
	handler := HandlerWithProvidersAndStateFile(map[string]Provider{"a": pa, "b": plain}, path)

	res := doReconcile(t, handler, `{}`)
	if res.status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.status)
	}
	if len(res.Errors) != 1 || res.Errors[0].Code != codeObserverUnavailable || res.Errors[0].Path != "/resources/1" {
		t.Fatalf("unexpected errors %+v", res.Errors)
	}
	if len(pa.observes) != 0 || len(pa.applies) != 0 {
		t.Fatalf("provider called despite failed precheck")
	}
}

func TestMultiReconcileAllNoopSucceedsWithEmptyRegistry(t *testing.T) {
	path := stateFilePath(t)
	handler := HandlerWithProvidersAndStateFile(map[string]Provider{}, path)
	res := doReconcile(t, handler, `{}`)
	if res.status != http.StatusOK || !res.Valid {
		t.Fatalf("got status=%d valid=%v errors=%v", res.status, res.Valid, res.Errors)
	}
	if res.Applied == nil || len(*res.Applied) != 0 {
		t.Fatalf("applied = %v, want empty", res.Applied)
	}
}
