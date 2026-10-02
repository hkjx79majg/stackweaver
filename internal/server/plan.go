package server

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strings"
)

// Error code reported when the plan request envelope or the prior state
// document is structurally invalid. Configuration findings keep the codes
// produced by the configuration validator and ordering passes.
const codeInvalidPlanRequest = "invalid_plan_request"

// planSnapshot is the before/after view of one resource: its type, its
// properties, and its dependency set normalized to ascending address order.
type planSnapshot struct {
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties"`
	DependsOn  []string       `json:"dependsOn"`
}

// planChange is one planned action. Creates carry only After, deletes only
// Before, updates both.
type planChange struct {
	Action  string        `json:"action"`
	Address string        `json:"address"`
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

// planResource is the side-agnostic internal form of a resource used for
// diffing. DependsOn is sorted ascending and duplicate-free.
type planResource struct {
	address    string
	typ        string
	properties map[string]any
	dependsOn  []string
}

// handleCreatePlan computes the diff between a desired configuration and a
// prior state. It is pure: no provider is contacted and nothing persists.
func handleCreatePlan(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"code": "method_not_allowed"},
		})
		return
	}

	doc, ok := decodeConfigurationBody(w, r)
	if !ok {
		return
	}

	changes, summary, errs := computePlan(doc)
	if len(errs) != 0 {
		sortValidationErrors(errs)
		writeValidation(w, http.StatusUnprocessableEntity, errs)
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

// computePlan validates the request envelope, the configuration, and the
// prior state independently, collecting every determinable problem. Only
// when all three are clean does it diff the two sides.
func computePlan(doc any) ([]planChange, planSummary, []validationError) {
	var errs []validationError
	add := func(code, message, path string) {
		errs = append(errs, validationError{Code: code, Message: message, Path: path})
	}

	root, ok := doc.(map[string]any)
	if !ok {
		add(codeInvalidPlanRequest, "request body must be a JSON object", "")
		return nil, planSummary{}, errs
	}
	for key := range root {
		if key != "configuration" && key != "priorState" {
			add(codeInvalidPlanRequest, fmt.Sprintf("unknown field %q", key), "/"+pointerEscape(key))
		}
	}
	cfgRaw, hasCfg := root["configuration"]
	if !hasCfg {
		add(codeInvalidPlanRequest, `missing required field "configuration"`, "")
	}
	stateRaw, hasState := root["priorState"]
	if !hasState {
		add(codeInvalidPlanRequest, `missing required field "priorState"`, "")
	}

	var cfgOrder []string
	var cfgResources []planResource
	if hasCfg {
		cfgErrs := validateConfiguration(cfgRaw)
		if len(cfgErrs) == 0 {
			// Dependencies are only interpreted once the base document is
			// valid, mirroring the order endpoint.
			var depErrs []validationError
			cfgOrder, depErrs = orderConfiguration(cfgRaw)
			cfgErrs = depErrs
		}
		for i := range cfgErrs {
			cfgErrs[i].Path = "/configuration" + cfgErrs[i].Path
		}
		errs = append(errs, cfgErrs...)
		if len(cfgErrs) == 0 {
			cfgResources = configPlanResources(cfgRaw)
		}
	}

	var stateOrder []string
	var stateResources []planResource
	if hasState {
		var stateErrs []validationError
		stateOrder, stateResources, stateErrs = validatePriorState(stateRaw)
		errs = append(errs, stateErrs...)
	}

	if len(errs) != 0 {
		return nil, planSummary{}, errs
	}
	changes, summary := diffPlan(cfgOrder, cfgResources, stateOrder, stateResources)
	return changes, summary, nil
}

// validatePriorState checks the shape of a prior state document and the
// semantics of its dependency references. State types and properties are not
// validated against the configuration's resource types: the state may keep
// resources whose types no longer exist on the desired side. Returns the
// deterministic topological order of the state and its resources.
func validatePriorState(raw any) ([]string, []planResource, []validationError) {
	var errs []validationError
	add := func(code, message, path string) {
		errs = append(errs, validationError{Code: code, Message: message, Path: path})
	}

	obj, ok := raw.(map[string]any)
	if !ok {
		add(codeInvalidPlanRequest, `"priorState" must be an object`, "/priorState")
		return nil, nil, errs
	}
	for key := range obj {
		if key != "resources" {
			add(codeInvalidPlanRequest, fmt.Sprintf("unknown field %q", key), "/priorState/"+pointerEscape(key))
		}
	}
	resRaw, has := obj["resources"]
	if !has {
		add(codeInvalidPlanRequest, `missing required field "resources"`, "/priorState")
		return nil, nil, errs
	}
	entries, ok := resRaw.([]any)
	if !ok {
		add(codeInvalidPlanRequest, `"resources" must be an array`, "/priorState/resources")
		return nil, nil, errs
	}

	addresses := make([]string, len(entries))
	known := make(map[string]bool, len(entries))
	for i, entry := range entries {
		entryPath := fmt.Sprintf("/priorState/resources/%d", i)
		entryObj, ok := entry.(map[string]any)
		if !ok {
			add(codeInvalidPlanRequest, "state resource entry must be an object", entryPath)
			continue
		}
		if address, ok := planStringField(entryObj, "address", entryPath, add); ok {
			if known[address] {
				add(codeDuplicateIdentifier, fmt.Sprintf("duplicate resource address %q", address), entryPath+"/address")
			} else {
				known[address] = true
				addresses[i] = address
			}
		}
		planStringField(entryObj, "type", entryPath, add)
		propsRaw, has := entryObj["properties"]
		if !has {
			add(codeInvalidPlanRequest, `missing required field "properties"`, entryPath)
		} else if _, ok := propsRaw.(map[string]any); !ok {
			add(codeInvalidPlanRequest, `"properties" must be an object`, entryPath+"/properties")
		}
	}
	// Dependencies are only interpreted once the state shape is valid.
	if len(errs) != 0 {
		return nil, nil, errs
	}

	dependsOn := make([][]string, len(entries))
	for i, entry := range entries {
		entryObj := entry.(map[string]any)
		rawDeps, present := entryObj["dependsOn"]
		if !present {
			continue
		}
		fieldPath := fmt.Sprintf("/priorState/resources/%d/dependsOn", i)
		arr, ok := rawDeps.([]any)
		if !ok {
			add(codeInvalidPlanRequest, `"dependsOn" must be an array of non-empty resource address strings`, fieldPath)
			continue
		}
		seen := map[string]bool{}
		for j, el := range arr {
			elPath := fmt.Sprintf("%s/%d", fieldPath, j)
			ref, isString := el.(string)
			if !isString || ref == "" {
				add(codeInvalidPlanRequest, "dependency must be a non-empty resource address string", elPath)
				continue
			}
			if seen[ref] {
				add(codeDuplicateDependency, fmt.Sprintf("duplicate dependency %q", ref), elPath)
				continue
			}
			seen[ref] = true
			switch {
			case ref == addresses[i]:
				add(codeSelfDependency, "resource must not depend on itself", elPath)
			case !known[ref]:
				add(codeUnknownDependency, fmt.Sprintf("unknown dependency %q", ref), elPath)
			default:
				dependsOn[i] = append(dependsOn[i], ref)
			}
		}
	}
	if len(errs) != 0 {
		return nil, nil, errs
	}

	if cyclic := cyclicResources(addresses, dependsOn); cyclic != nil {
		sort.Strings(cyclic)
		add(codeDependencyCycle, "dependency cycle: "+strings.Join(cyclic, ", "), "/priorState/resources")
		return nil, nil, errs
	}

	resources := make([]planResource, len(entries))
	for i, entry := range entries {
		entryObj := entry.(map[string]any)
		deps := append([]string{}, dependsOn[i]...)
		sort.Strings(deps)
		resources[i] = planResource{
			address:    addresses[i],
			typ:        entryObj["type"].(string),
			properties: entryObj["properties"].(map[string]any),
			dependsOn:  deps,
		}
	}
	return topologicalOrder(addresses, dependsOn), resources, nil
}

// planStringField extracts a required non-empty string field of a prior
// state entry, reporting invalid_plan_request for a missing, non-string, or
// empty value.
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

// configPlanResources extracts the diff-relevant view of an already-valid
// configuration document.
func configPlanResources(raw any) []planResource {
	entries := raw.(map[string]any)["resources"].([]any)
	resources := make([]planResource, len(entries))
	for i, entry := range entries {
		obj := entry.(map[string]any)
		deps := []string{}
		if rawDeps, ok := obj["dependsOn"].([]any); ok {
			for _, el := range rawDeps {
				deps = append(deps, el.(string))
			}
			sort.Strings(deps)
		}
		resources[i] = planResource{
			address:    obj["address"].(string),
			typ:        obj["type"].(string),
			properties: obj["properties"].(map[string]any),
			dependsOn:  deps,
		}
	}
	return resources
}

// diffPlan aligns desired and prior resources by address. Deletes come
// first, in the reverse of the prior state's topological order, so dependents
// are removed before what they depended on; creates and updates follow in
// the configuration's deterministic topological order.
func diffPlan(cfgOrder []string, cfgResources []planResource, stateOrder []string, stateResources []planResource) ([]planChange, planSummary) {
	cfgByAddr := make(map[string]planResource, len(cfgResources))
	for _, res := range cfgResources {
		cfgByAddr[res.address] = res
	}
	stateByAddr := make(map[string]planResource, len(stateResources))
	for _, res := range stateResources {
		stateByAddr[res.address] = res
	}

	changes := make([]planChange, 0, len(cfgResources)+len(stateResources))
	var summary planSummary

	for i := len(stateOrder) - 1; i >= 0; i-- {
		address := stateOrder[i]
		if _, desired := cfgByAddr[address]; desired {
			continue
		}
		before := snapshotOf(stateByAddr[address])
		changes = append(changes, planChange{Action: "delete", Address: address, Before: &before})
		summary.Delete++
	}

	for _, address := range cfgOrder {
		desired := cfgByAddr[address]
		after := snapshotOf(desired)
		current, exists := stateByAddr[address]
		if !exists {
			changes = append(changes, planChange{Action: "create", Address: address, After: &after})
			summary.Create++
			continue
		}
		if resourcesEqual(desired, current) {
			summary.Noop++
			continue
		}
		before := snapshotOf(current)
		changes = append(changes, planChange{Action: "update", Address: address, Before: &before, After: &after})
		summary.Update++
	}

	return changes, summary
}

func snapshotOf(res planResource) planSnapshot {
	deps := make([]string, len(res.dependsOn))
	copy(deps, res.dependsOn)
	return planSnapshot{Type: res.typ, Properties: res.properties, DependsOn: deps}
}

// resourcesEqual compares type, properties, and the (already normalized)
// dependency set.
func resourcesEqual(a, b planResource) bool {
	if a.typ != b.typ {
		return false
	}
	if !jsonValueEqual(a.properties, b.properties) {
		return false
	}
	if len(a.dependsOn) != len(b.dependsOn) {
		return false
	}
	for i := range a.dependsOn {
		if a.dependsOn[i] != b.dependsOn[i] {
			return false
		}
	}
	return true
}

// jsonValueEqual compares two decoded JSON values semantically: object key
// order is insignificant, numbers compare by value (1 equals 1.0), and array
// order is significant.
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
		for key, va := range av {
			vb, present := bv[key]
			if !present || !jsonValueEqual(va, vb) {
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
		// nil, bool, and string compare directly.
		return a == b
	}
}

// jsonNumberEqual compares two JSON number literals by exact numeric value,
// so 1, 1.0, and 1e0 are all equal.
func jsonNumberEqual(a, b json.Number) bool {
	ra, okA := new(big.Rat).SetString(a.String())
	rb, okB := new(big.Rat).SetString(b.String())
	if !okA || !okB {
		return a.String() == b.String()
	}
	return ra.Cmp(rb) == 0
}
