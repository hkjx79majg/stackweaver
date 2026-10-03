package server

// providerRouter resolves the Provider responsible for one resource type. The
// single-provider mode mirrors the baseline constructors: every type resolves
// to the same injected Provider, which may be nil. The multi-provider mode
// dispatches by resource type through a registry built from a
// map[string]Provider; empty type keys and nil values are dropped at
// construction, and one instance may serve any number of types.
type providerRouter struct {
	single Provider
	byType map[string]Provider
	multi  bool
}

// singleRouter routes every type to provider, exactly like the baseline
// HandlerWithProvider wiring.
func singleRouter(provider Provider) providerRouter {
	return providerRouter{single: provider}
}

// multiRouter builds a type-dispatching router from providers. Entries with
// an empty type key or a nil Provider are treated as unregistered.
func multiRouter(providers map[string]Provider) providerRouter {
	byType := make(map[string]Provider, len(providers))
	for typ, provider := range providers {
		if typ == "" || provider == nil {
			continue
		}
		byType[typ] = provider
	}
	return providerRouter{byType: byType, multi: true}
}

// forType returns the Provider responsible for typ, or nil when the type is
// unregistered. In single-provider mode it always returns the injected
// Provider regardless of typ.
func (r providerRouter) forType(typ string) Provider {
	if !r.multi {
		return r.single
	}
	return r.byType[typ]
}

// forChange routes one planned change: create and update go to the Provider
// of the after snapshot's type (so a type-changing update is executed by the
// target type's Provider), delete goes to the before snapshot's type.
func (r providerRouter) forChange(change planChange) Provider {
	if !r.multi {
		return r.single
	}
	if change.Action == "delete" {
		return r.byType[change.Before.Type]
	}
	return r.byType[change.After.Type]
}

// unroutedChange reports the global index and resource type of the first
// change in the ordered list whose type has no registered Provider. It is the
// multi-provider preflight: ok=false means every change can be dispatched.
func (r providerRouter) unroutedChange(changes []planChange) (index int, typ string, ok bool) {
	for i, change := range changes {
		var typ string
		if change.Action == "delete" {
			typ = change.Before.Type
		} else {
			typ = change.After.Type
		}
		if r.byType[typ] == nil {
			return i, typ, true
		}
	}
	return 0, "", false
}
