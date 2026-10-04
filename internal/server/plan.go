package server

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sort"
)

// Error code reported when the plan request envelope or the prior state
// document is structurally invalid.
const codeInvalidPlanRequest = "invalid_plan_request"

// planSnapshot is the normalized view of one resource at one side of a
// change: before for the prior state, after for the configuration. It is an
// alias of the exported Snapshot handed to Provider implementations so that
// the read-only plan endpoint and the apply endpoint share one shape.
type planSnapshot = Snapshot

// planChange is one planned action. Create carries only After, delete only
// Before, update carries both.
type planChange struct {
	Address string        `json:"address"`
	Action  string        `json:"action"`
	Before  *planSnapshot `json:"before,omitempty"`
	After   *planSnapshot `json:"after,omitempty"`
}

type planSummary struct {
	Create int `json:"create"`
	Update int `json:"update"`
	Delete int `json:"delete"`
	Noop   int `json:"noop"`
}

type planResponse struct {
	Valid   bool              `json:"valid"`
	Errors  []validationError `json:"errors"`
	Changes []planChange      `json:"changes"`
	Summary planSummary       `json:"summary"`
}

// planResource is the plan-relevant view of one resource, extracted from an
// already-validated configuration or prior state document.
type planResource struct {
	address    string
	typ        string
	properties map[string]any
	dependsOn  []string
}

// handlePlanConfiguration computes the diff between a desired configuration
// and a prior state. It only computes the plan; nothing is applied or
// persisted.
func handlePlan(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.Method != http.MethodPost {
		rejectMethodNotAllowed(w)
		return
	}

	changes, summary, _, ok := preparePlan(w, r)
	if !ok {
		return
	}

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(planResponse{
		Valid:   true,
		Errors:  []validationError{},
		Changes: changes,
		Summary: summary,
	})
}

// preparePlan runs the request through the same pipeline the read-only plan
// endpoint uses: decode the body, validate the envelope, configuration,
// dependencies and prior state, then compute the ordered diff. On any client
// failure it writes the 400/422 response itself and returns ok=false, so the
// caller never invokes a Provider for an invalid request. On success it also
// returns the validated prior-state resources, used to seed the resulting
// state of an apply run.
func preparePlan(w http.ResponseWriter, r *http.Request) (changes []planChange, summary planSummary, stateResources []planResource, ok bool) {
	doc, ok := decodeConfigurationBody(w, r)
	if !ok {
		return nil, planSummary{}, nil, false
	}

	configRaw, stateRaw, ok := checkPlanEnvelope(w, doc)
	if !ok {
		return nil, planSummary{}, nil, false
	}

	// Configuration and prior state are validated independently; their
	// findings are merged and sorted together. Configuration findings keep
	// their codes and gain a /configuration path prefix. A valid
	// configuration is resolved — variable references replaced by their
	// effective values — before ordering and diffing, so plans, snapshots,
	// and state only ever see resolved properties.
	var errs []validationError
	var configResources []planResource
	var configOrder []string
	if baseErrs := validateConfiguration(configRaw); len(baseErrs) != 0 {
		errs = appendPrefixedErrors(errs, baseErrs, "/configuration")
	} else {
		resolved := resolveConfigurationVariables(configRaw)
		if order, depErrs := orderConfiguration(resolved); len(depErrs) != 0 {
			errs = appendPrefixedErrors(errs, depErrs, "/configuration")
		} else {
			configResources = configurationResources(resolved)
			configOrder = order
		}
	}

	stateResources, stateOrder, stateErrs := checkPriorState(stateRaw)
	errs = append(errs, stateErrs...)

	if len(errs) != 0 {
		sortValidationErrors(errs)
		writeValidation(w, http.StatusUnprocessableEntity, errs)
		return nil, planSummary{}, nil, false
	}

	changes, summary = computePlan(configResources, configOrder, stateResources, stateOrder)
	return changes, summary, stateResources, true
}

// rejectMethodNotAllowed writes the shared 405 response for the POST-only
// planning and apply endpoints: Allow: POST and a method_not_allowed code.
func rejectMethodNotAllowed(w http.ResponseWriter) {
	w.Header().Set("Allow", http.MethodPost)
	w.WriteHeader(http.StatusMethodNotAllowed)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"code": "method_not_allowed"},
	})
}

// checkPlanEnvelope validates the request envelope: a JSON object holding
// exactly the configuration and priorState fields. On failure it writes the
// 422 response and returns ok=false.
func checkPlanEnvelope(w http.ResponseWriter, doc any) (configuration, priorState any, ok bool) {
	var errs []validationError
	add := func(code, message, path string) {
		errs = append(errs, validationError{Code: code, Message: message, Path: path})
	}

	root, isObject := doc.(map[string]any)
	if !isObject {
		add(codeInvalidPlanRequest, "request body must be a JSON object", "")
	} else {
		for key := range root {
			if key != "configuration" && key != "priorState" {
				add(codeInvalidPlanRequest, fmt.Sprintf("unknown field %q", key), "/"+pointerEscape(key))
			}
		}
		var hasConfig, hasState bool
		configuration, hasConfig = root["configuration"]
		priorState, hasState = root["priorState"]
		if !hasConfig {
			add(codeInvalidPlanRequest, `missing required field "configuration"`, "")
		}
		if !hasState {
			add(codeInvalidPlanRequest, `missing required field "priorState"`, "")
		}
	}

	if len(errs) != 0 {
		sortValidationErrors(errs)
		writeValidation(w, http.StatusUnprocessableEntity, errs)
		return nil, nil, false
	}
	return configuration, priorState, true
}

// appendPrefixedErrors appends errs to dst with every path rooted at prefix,
// so findings from an embedded document point into the request envelope.
func appendPrefixedErrors(dst []validationError, errs []validationError, prefix string) []validationError {
	for _, e := range errs {
		e.Path = prefix + e.Path
		dst = append(dst, e)
	}
	return dst
}

// configurationResources extracts the plan-relevant view of an
// already-validated configuration document.
func configurationResources(doc any) []planResource {
	root := doc.(map[string]any)
	entries := root["resources"].([]any)
	resources := make([]planResource, 0, len(entries))
	for _, entry := range entries {
		obj := entry.(map[string]any)
		var deps []string
		if raw, ok := obj["dependsOn"].([]any); ok {
			for _, el := range raw {
				deps = append(deps, el.(string))
			}
		}
		resources = append(resources, planResource{
			address:    obj["address"].(string),
			typ:        obj["type"].(string),
			properties: obj["properties"].(map[string]any),
			dependsOn:  deps,
		})
	}
	return resources
}

// checkPriorState validates the priorState document: an object with a
// resources array whose entries have a non-empty unique address, a non-empty
// type, a properties object, and an optional dependsOn. State types are not
// checked against the configuration's resource types. Shape problems are
// reported as invalid_plan_request and suppress dependency interpretation;
// dependency problems reuse the configuration dependency codes.
func checkPriorState(raw any) ([]planResource, []string, []validationError) {
	const base = "/priorState"
	var errs []validationError
	add := func(code, message, path string) {
		errs = append(errs, validationError{Code: code, Message: message, Path: path})
	}

	obj, ok := raw.(map[string]any)
	if !ok {
		add(codeInvalidPlanRequest, `"priorState" must be an object`, base)
		return nil, nil, errs
	}
	resourcesRaw, hasResources := obj["resources"]
	if !hasResources {
		add(codeInvalidPlanRequest, `missing required field "resources"`, base)
		return nil, nil, errs
	}
	entries, ok := resourcesRaw.([]any)
	if !ok {
		add(codeInvalidPlanRequest, `"resources" must be an array`, base+"/resources")
		return nil, nil, errs
	}

	resources := make([]planResource, len(entries))
	seenAddresses := map[string]bool{}
	for i, entry := range entries {
		entryPath := fmt.Sprintf("%s/resources/%d", base, i)
		entryObj, ok := entry.(map[string]any)
		if !ok {
			add(codeInvalidPlanRequest, "state resource entry must be an object", entryPath)
			continue
		}
		address, addressOK := planStringField(entryObj, "address", entryPath, add)
		if addressOK {
			if seenAddresses[address] {
				add(codeDuplicateIdentifier, fmt.Sprintf("duplicate resource address %q", address), entryPath+"/address")
				addressOK = false
			} else {
				seenAddresses[address] = true
			}
		}
		typ, typeOK := planStringField(entryObj, "type", entryPath, add)
		propsRaw, hasProps := entryObj["properties"]
		props, propsOK := propsRaw.(map[string]any)
		switch {
		case !hasProps:
			add(codeInvalidPlanRequest, `missing required field "properties"`, entryPath)
		case !propsOK:
			add(codeInvalidPlanRequest, `"properties" must be an object`, entryPath+"/properties")
		}
		if !addressOK || !typeOK || !hasProps || !propsOK {
			continue
		}
		resources[i] = planResource{address: address, typ: typ, properties: props}
	}
	if len(errs) != 0 {
		return nil, nil, errs
	}

	addresses := make([]string, len(entries))
	for i, resource := range resources {
		addresses[i] = resource.address
	}
	dependsOn, order, depErrs := resolveDependencies(entries, addresses, base+"/resources")
	if len(depErrs) != 0 {
		return nil, nil, depErrs
	}
	for i := range resources {
		resources[i].dependsOn = dependsOn[i]
	}
	return resources, order, nil
}

// planStringField extracts a required non-empty string field of a prior
// state entry, reporting invalid_plan_request otherwise.
func planStringField(obj map[string]any, field, path string, add func(code, message, path string)) (string, bool) {
	raw, ok := obj[field]
	if !ok {
		add(codeInvalidPlanRequest, fmt.Sprintf("missing required field %q", field), path)
		return "", false
	}
	s, ok := raw.(string)
	if !ok {
		add(codeInvalidPlanRequest, fmt.Sprintf("%q must be a string", field), path+"/"+field)
		return "", false
	}
	if s == "" {
		add(codeInvalidPlanRequest, fmt.Sprintf("%q must not be empty", field), path+"/"+field)
		return "", false
	}
	return s, true
}

// computePlan aligns desired and prior resources by address. Deletes are
// listed in the reverse of the prior state's topological order; creates and
// updates follow the configuration's deterministic topological order.
// Resources equal on both sides are counted as noop and produce no change.
func computePlan(configResources []planResource, configOrder []string, stateResources []planResource, stateOrder []string) ([]planChange, planSummary) {
	configByAddress := make(map[string]planResource, len(configResources))
	for _, resource := range configResources {
		configByAddress[resource.address] = resource
	}
	stateByAddress := make(map[string]planResource, len(stateResources))
	for _, resource := range stateResources {
		stateByAddress[resource.address] = resource
	}

	actions := make(map[string]string, len(configResources)+len(stateResources))
	var summary planSummary
	for _, resource := range configResources {
		prior, existed := stateByAddress[resource.address]
		switch {
		case !existed:
			actions[resource.address] = "create"
			summary.Create++
		case resourceEqual(resource, prior):
			summary.Noop++
		default:
			actions[resource.address] = "update"
			summary.Update++
		}
	}
	for _, resource := range stateResources {
		if _, desired := configByAddress[resource.address]; !desired {
			actions[resource.address] = "delete"
			summary.Delete++
		}
	}

	changes := make([]planChange, 0, len(actions))
	for i := len(stateOrder) - 1; i >= 0; i-- {
		address := stateOrder[i]
		if actions[address] != "delete" {
			continue
		}
		before := snapshotOf(stateByAddress[address])
		changes = append(changes, planChange{Address: address, Action: "delete", Before: &before})
	}
	for _, address := range configOrder {
		action := actions[address]
		if action != "create" && action != "update" {
			continue
		}
		after := snapshotOf(configByAddress[address])
		change := planChange{Address: address, Action: action, After: &after}
		if action == "update" {
			before := snapshotOf(stateByAddress[address])
			change.Before = &before
		}
		changes = append(changes, change)
	}
	return changes, summary
}

// snapshotOf renders a resource as a change snapshot with its dependencies
// normalized to ascending order.
func snapshotOf(resource planResource) planSnapshot {
	deps := make([]string, len(resource.dependsOn))
	copy(deps, resource.dependsOn)
	sort.Strings(deps)
	return planSnapshot{
		Type:       resource.typ,
		Properties: resource.properties,
		DependsOn:  deps,
	}
}

// resourceEqual reports whether a desired and a prior resource are
// equivalent: same type, deeply equal properties, and equal dependency sets.
func resourceEqual(desired, prior planResource) bool {
	if desired.typ != prior.typ {
		return false
	}
	if !jsonValueEqual(desired.properties, prior.properties) {
		return false
	}
	return stringSetEqual(desired.dependsOn, prior.dependsOn)
}

// jsonValueEqual compares two decoded JSON values. Object key order is
// ignored, numbers compare by numeric value, and arrays are ordered.
func jsonValueEqual(a, b any) bool {
	switch av := a.(type) {
	case json.Number:
		bv, ok := b.(json.Number)
		return ok && jsonNumberEqual(av, bv)
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for key, value := range av {
			other, present := bv[key]
			if !present || !jsonValueEqual(value, other) {
				return false
			}
		}
		return true
	case []any:
		bv, ok := b.([]any)
		if !ok || len(av) != len(bv) {
			return false
		}
		for i := range av {
			if !jsonValueEqual(av[i], bv[i]) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}

// jsonNumberEqual compares two JSON number literals by numeric value, so
// 1, 1.0, and 1e0 are all equal.
func jsonNumberEqual(a, b json.Number) bool {
	ra, okA := new(big.Rat).SetString(a.String())
	rb, okB := new(big.Rat).SetString(b.String())
	if okA && okB {
		return ra.Cmp(rb) == 0
	}
	return a.String() == b.String()
}

// stringSetEqual compares two duplicate-free string lists as sets.
func stringSetEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, s := range a {
		counts[s]++
	}
	for _, s := range b {
		counts[s]--
		if counts[s] < 0 {
			return false
		}
	}
	return true
}
