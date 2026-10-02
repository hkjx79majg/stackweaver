package server

import (
	"container/heap"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Error codes reported by the dependency order endpoint.
const (
	codeUnknownDependency   = "unknown_dependency"
	codeSelfDependency      = "self_dependency"
	codeDuplicateDependency = "duplicate_dependency"
	codeDependencyCycle     = "dependency_cycle"
)

type orderResponse struct {
	Valid  bool              `json:"valid"`
	Errors []validationError `json:"errors"`
	// Order is a pointer so that a successful response with an empty
	// resources list serializes as [], while failure responses omit the
	// field entirely.
	Order *[]string `json:"order,omitempty"`
}

// handleOrderConfiguration returns a deterministic topological order over
// the resources of an already structurally valid configuration document.
// Nothing is created or persisted.
func handleOrderConfiguration(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	doc, ok := decodeConfigurationRequest(w, r)
	if !ok {
		return
	}

	// Dependency semantics are only interpreted once the base configuration
	// is valid; base failures reuse the validate response verbatim.
	if baseErrs := validateConfiguration(doc); len(baseErrs) > 0 {
		sortValidationErrors(baseErrs)
		writeValidation(w, http.StatusUnprocessableEntity, baseErrs)
		return
	}

	root := doc.(map[string]any)
	entries := root["resources"].([]any)
	addresses := make([]string, len(entries))
	for i, entry := range entries {
		addresses[i] = entry.(map[string]any)["address"].(string)
	}
	known := make(map[string]bool, len(addresses))
	for _, addr := range addresses {
		known[addr] = true
	}

	var errs []validationError
	// deps[i] holds usable dependency edges: references that name a known,
	// distinct resource, each at most once. A malformed dependsOn value
	// contributes no edges.
	deps := make([][]string, len(entries))
	for i, entry := range entries {
		obj := entry.(map[string]any)
		raw, present := obj["dependsOn"]
		if !present {
			continue
		}
		base := fmt.Sprintf("/resources/%d/dependsOn", i)
		arr, ok := raw.([]any)
		if !ok {
			errs = append(errs, validationError{
				Code:    codeInvalidDocument,
				Message: `"dependsOn" must be an array of non-empty resource address strings`,
				Path:    base,
			})
			continue
		}
		seen := map[string]bool{}
		for j, el := range arr {
			elPath := fmt.Sprintf("%s/%d", base, j)
			ref, isString := el.(string)
			if !isString || ref == "" {
				errs = append(errs, validationError{
					Code:    codeInvalidDocument,
					Message: "dependsOn entries must be non-empty resource address strings",
					Path:    elPath,
				})
				continue
			}
			switch {
			case ref == addresses[i]:
				errs = append(errs, validationError{
					Code:    codeSelfDependency,
					Message: fmt.Sprintf("resource %q must not depend on itself", ref),
					Path:    elPath,
				})
			case !known[ref]:
				errs = append(errs, validationError{
					Code:    codeUnknownDependency,
					Message: fmt.Sprintf("unknown dependency address %q", ref),
					Path:    elPath,
				})
			default:
				if seen[ref] {
					errs = append(errs, validationError{
						Code:    codeDuplicateDependency,
						Message: fmt.Sprintf("duplicate dependency %q", ref),
						Path:    elPath,
					})
					continue
				}
				seen[ref] = true
				deps[i] = append(deps[i], ref)
			}
		}
	}

	if len(errs) > 0 {
		// Reference errors short-circuit cycle detection.
		sortValidationErrors(errs)
		writeOrder(w, http.StatusUnprocessableEntity, errs, nil)
		return
	}

	order, cycle := topologicalOrder(addresses, deps)
	if cycle != nil {
		writeOrder(w, http.StatusUnprocessableEntity, []validationError{{
			Code:    codeDependencyCycle,
			Message: "dependency cycle: " + strings.Join(cycle, ", "),
			Path:    "/resources",
		}}, nil)
		return
	}
	writeOrder(w, http.StatusOK, nil, order)
}

func writeOrder(w http.ResponseWriter, status int, errs []validationError, order []string) {
	if errs == nil {
		errs = []validationError{}
	}
	res := orderResponse{
		Valid:  len(errs) == 0,
		Errors: errs,
	}
	if status == http.StatusOK {
		if order == nil {
			order = []string{}
		}
		res.Order = &order
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(res)
}

// topologicalOrder returns the resource addresses in dependency order: every
// dependency precedes the resource that depends on it, and when more than one
// resource is ready the smallest address (Unicode code point order) is taken
// first. If the graph contains a cycle, order is nil and cycle lists exactly
// the resources that take part in a cycle, sorted by address; resources that
// merely depend (even indirectly) on a cycle are excluded.
func topologicalOrder(addresses []string, deps [][]string) (order []string, cycle []string) {
	n := len(addresses)
	index := make(map[string]int, n)
	for i, addr := range addresses {
		index[addr] = i
	}

	// indeg[i] counts incoming edges per node. Every remaining reference is
	// unique within its source list thanks to duplicate detection.
	indeg := make([]int, n)
	// dependents[dep] lists resources depending on dep.
	dependents := make([][]int, n)
	for i, list := range deps {
		for _, ref := range list {
			j := index[ref]
			indeg[i]++
			dependents[j] = append(dependents[j], i)
		}
	}

	ready := &addressHeap{addresses: addresses}
	for i := 0; i < n; i++ {
		if indeg[i] == 0 {
			heap.Push(ready, i)
		}
	}

	emitted := make([]bool, n)
	order = make([]string, 0, n)
	for ready.Len() > 0 {
		i := heap.Pop(ready).(int)
		emitted[i] = true
		order = append(order, addresses[i])
		for _, j := range dependents[i] {
			indeg[j]--
			if indeg[j] == 0 {
				heap.Push(ready, j)
			}
		}
	}

	if len(order) == n {
		return order, nil
	}

	// Nodes never emitted are exactly those entangled with a cycle or
	// feeding into one. Restrict the graph to them, then take the
	// transitive closure: a node is genuinely on a cycle iff it can reach
	// itself through the induced edges.
	remaining := make([]int, 0, n)
	for i := 0; i < n; i++ {
		if !emitted[i] {
			remaining = append(remaining, i)
		}
	}
	remIndex := make(map[int]int, len(remaining))
	for k, i := range remaining {
		remIndex[i] = k
	}
	m := len(remaining)
	reaches := make([]map[int]bool, m)
	for k := 0; k < m; k++ {
		reaches[k] = map[int]bool{}
	}
	for k, i := range remaining {
		for _, ref := range deps[i] {
			if j, ok := remIndex[index[ref]]; ok {
				reaches[k][j] = true
			}
		}
	}
	// Floyd-Warshall over the induced subgraph.
	for k := 0; k < m; k++ {
		for i := 0; i < m; i++ {
			if !reaches[i][k] {
				continue
			}
			for j := range reaches[k] {
				reaches[i][j] = true
			}
		}
	}

	cyclic := make([]string, 0)
	for k, i := range remaining {
		if reaches[k][k] {
			cyclic = append(cyclic, addresses[i])
		}
	}
	sort.Strings(cyclic)
	return nil, cyclic
}

// addressHeap orders node indices by their resource address so that the
// smallest available address is always emitted next.
type addressHeap struct {
	nodes     []int
	addresses []string
}

func (h *addressHeap) Len() int { return len(h.nodes) }

func (h *addressHeap) Less(i, j int) bool {
	return h.addresses[h.nodes[i]] < h.addresses[h.nodes[j]]
}

func (h *addressHeap) Swap(i, j int) {
	h.nodes[i], h.nodes[j] = h.nodes[j], h.nodes[i]
}

func (h *addressHeap) Push(x any) {
	h.nodes = append(h.nodes, x.(int))
}

func (h *addressHeap) Pop() any {
	n := len(h.nodes)
	x := h.nodes[n-1]
	h.nodes = h.nodes[:n-1]
	return x
}
