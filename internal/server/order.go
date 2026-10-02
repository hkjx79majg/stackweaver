package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

type orderResponse struct {
	Valid  bool              `json:"valid"`
	Errors []validationError `json:"errors"`
	Order  []string          `json:"order"`
}

// handleOrderConfiguration returns a deterministic topological order over
// the resources of an already-valid configuration document.
func handleOrderConfiguration(w http.ResponseWriter, r *http.Request) {
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

	// Dependencies are only interpreted once the base document is valid.
	baseErrs := validateConfiguration(doc)
	if len(baseErrs) != 0 {
		sortValidationErrors(baseErrs)
		writeValidation(w, http.StatusUnprocessableEntity, baseErrs)
		return
	}

	order, depErrs := orderConfiguration(doc)
	if len(depErrs) != 0 {
		sortValidationErrors(depErrs)
		writeValidation(w, http.StatusUnprocessableEntity, depErrs)
		return
	}

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(orderResponse{
		Valid:  true,
		Errors: []validationError{},
		Order:  order,
	})
}

// orderConfiguration interprets the dependsOn fields of a validated document.
// It returns every dependency problem in one pass; dependency problems take
// precedence over cycle detection, and cycles take precedence over ordering.
func orderConfiguration(doc any) ([]string, []validationError) {
	root := doc.(map[string]any)
	resourceEntries := root["resources"].([]any)

	addresses := make([]string, len(resourceEntries))
	for i, entry := range resourceEntries {
		addresses[i] = entry.(map[string]any)["address"].(string)
	}

	_, order, errs := resolveDependencies(resourceEntries, addresses, "/resources")
	if len(errs) != 0 {
		return nil, errs
	}
	return order, nil
}

// resolveDependencies interprets the dependsOn fields of resource entries
// whose addresses are already known to be non-empty and unique. resourcesPath
// is the JSON Pointer of the entries array, used as the base of error paths.
// It returns the per-entry dependency lists together with a deterministic
// topological order, or every dependency problem found; dependency problems
// take precedence over cycle detection, and cycles over ordering.
func resolveDependencies(entries []any, addresses []string, resourcesPath string) ([][]string, []string, []validationError) {
	known := make(map[string]bool, len(addresses))
	for _, address := range addresses {
		known[address] = true
	}

	var errs []validationError
	add := func(code, message, path string) {
		errs = append(errs, validationError{Code: code, Message: message, Path: path})
	}
	dependsOn := make([][]string, len(entries))
	for i, entry := range entries {
		obj := entry.(map[string]any)
		raw, present := obj["dependsOn"]
		if !present {
			continue
		}
		fieldPath := fmt.Sprintf("%s/%d/dependsOn", resourcesPath, i)
		arr, ok := raw.([]any)
		if !ok {
			add(codeInvalidDocument, `"dependsOn" must be an array of non-empty resource address strings`, fieldPath)
			continue
		}
		seen := map[string]bool{}
		for j, el := range arr {
			elPath := fmt.Sprintf("%s/%d", fieldPath, j)
			ref, isString := el.(string)
			if !isString || ref == "" {
				add(codeInvalidDocument, "dependency must be a non-empty resource address string", elPath)
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
		errs = append(errs, validationError{
			Code:    codeDependencyCycle,
			Message: "dependency cycle: " + strings.Join(cyclic, ", "),
			Path:    resourcesPath,
		})
		return nil, nil, errs
	}

	return dependsOn, topologicalOrder(addresses, dependsOn), nil
}

// topologicalOrder performs Kahn's algorithm, always selecting the available
// resource with the smallest address (Unicode code point order), so documents
// differing only in equivalent dependsOn permutations order identically.
func topologicalOrder(addresses []string, dependsOn [][]string) []string {
	dependents := make(map[string][]string, len(addresses))
	indegree := make(map[string]int, len(addresses))
	for _, address := range addresses {
		indegree[address] = 0
	}
	for i, deps := range dependsOn {
		indegree[addresses[i]] = len(deps)
		for _, dep := range deps {
			dependents[dep] = append(dependents[dep], addresses[i])
		}
	}

	var ready []string
	for _, address := range addresses {
		if indegree[address] == 0 {
			ready = append(ready, address)
		}
	}

	order := make([]string, 0, len(addresses))
	for len(ready) > 0 {
		sort.Strings(ready)
		next := ready[0]
		ready = ready[1:]
		order = append(order, next)
		for _, dependent := range dependents[next] {
			indegree[dependent]--
			if indegree[dependent] == 0 {
				ready = append(ready, dependent)
			}
		}
	}
	return order
}

// cyclicResources returns the addresses that actually lie on a dependency
// cycle. Resources that merely depend, directly or indirectly, on a cycle
// without being part of one are excluded. Self-loops cannot occur because
// self dependencies are rejected beforehand.
func cyclicResources(addresses []string, dependsOn [][]string) []string {
	index := make(map[string]int, len(addresses))
	for i, address := range addresses {
		index[address] = i
	}

	// Tarjan's strongly connected components algorithm, iteratively to
	// avoid recursion-depth concerns on long dependency chains.
	indices := make([]int, len(addresses))
	lowlink := make([]int, len(addresses))
	onStack := make([]bool, len(addresses))
	for i := range indices {
		indices[i] = -1
	}
	var stack []int
	var nextIndex int
	var cyclic []string

	type frame struct {
		v    int
		edge int
	}
	for root := range addresses {
		if indices[root] != -1 {
			continue
		}
		indices[root] = nextIndex
		lowlink[root] = nextIndex
		nextIndex++
		stack = append(stack, root)
		onStack[root] = true
		work := []frame{{v: root}}
		for len(work) > 0 {
			top := len(work) - 1
			v := work[top].v
			if work[top].edge < len(dependsOn[v]) {
				w := index[dependsOn[v][work[top].edge]]
				work[top].edge++
				if indices[w] == -1 {
					indices[w] = nextIndex
					lowlink[w] = nextIndex
					nextIndex++
					stack = append(stack, w)
					onStack[w] = true
					work = append(work, frame{v: w})
					continue
				}
				if onStack[w] && indices[w] < lowlink[v] {
					lowlink[v] = indices[w]
				}
				continue
			}
			if lowlink[v] == indices[v] {
				var component []string
				for {
					w := stack[len(stack)-1]
					stack = stack[:len(stack)-1]
					onStack[w] = false
					component = append(component, addresses[w])
					if w == v {
						break
					}
				}
				// With self-loops rejected, a component of two or more
				// nodes is exactly a cycle.
				if len(component) > 1 {
					cyclic = append(cyclic, component...)
				}
			}
			work = work[:len(work)-1]
			if len(work) > 0 {
				if parent := work[len(work)-1].v; lowlink[v] < lowlink[parent] {
					lowlink[parent] = lowlink[v]
				}
			}
		}
	}
	return cyclic
}
