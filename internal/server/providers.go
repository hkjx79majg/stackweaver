package server

import (
	"fmt"
	"net/http"
)

// providerSource abstracts where the Providers and Observers for a run come
// from: either one injected Provider serving every resource type, or a
// registry dispatching each change and resource to the Provider registered
// for its type. The two modes differ in how routing failures are reported —
// the single-Provider mode keeps the baseline's exactly one pre-execution
// check, while the registry mode prechecks every change or resource and
// indexes the failure into the plan or the address-sorted resource list.
type providerSource interface {
	// providerFor returns the Provider registered for typ.
	providerFor(typ string) (Provider, bool)
	// applyUnavailable builds the error reported when a change for typ at
	// global plan index i cannot be routed to any Provider.
	applyUnavailable(typ string, i int) validationError
	// checkObserverPreLock runs the mode's Observer capability check that
	// happens before the state lock is taken. It writes the response and
	// returns false when the run must abort; message phrases the failure.
	checkObserverPreLock(w http.ResponseWriter, message string) bool
	// observerFor returns the Observer serving resource, or the precheck
	// failure reported at ascending resource index i.
	observerFor(resource planResource, i int) (Observer, *validationError)
}

// singleSource routes every resource type to one injected Provider, exactly
// like the baseline wiring.
type singleSource struct {
	provider Provider
}

func (s singleSource) providerFor(string) (Provider, bool) {
	return s.provider, s.provider != nil
}

func (s singleSource) applyUnavailable(string, int) validationError {
	return validationError{
		Code:    codeProviderUnavailable,
		Message: "apply requires a provider, but none is configured",
		Path:    "",
	}
}

func (s singleSource) checkObserverPreLock(w http.ResponseWriter, message string) bool {
	if observerOf(s.provider) == nil {
		writeValidation(w, http.StatusServiceUnavailable, []validationError{{
			Code:    codeObserverUnavailable,
			Message: message,
			Path:    "",
		}})
		return false
	}
	return true
}

func (s singleSource) observerFor(planResource, int) (Observer, *validationError) {
	// checkObserverPreLock already established the capability.
	return observerOf(s.provider), nil
}

// multiSource routes each resource type to the Provider registered for it.
// The same instance may serve any number of types.
type multiSource struct {
	providers map[string]Provider
}

// newMultiSource freezes the registry at construction time. Entries with an
// empty type key or a nil Provider are not registered, so a lookup for them
// fails exactly like a type that was never mentioned.
func newMultiSource(providers map[string]Provider) multiSource {
	registered := make(map[string]Provider, len(providers))
	for typ, provider := range providers {
		if typ == "" || provider == nil {
			continue
		}
		registered[typ] = provider
	}
	return multiSource{providers: registered}
}

func (s multiSource) providerFor(typ string) (Provider, bool) {
	provider, ok := s.providers[typ]
	return provider, ok
}

func (s multiSource) applyUnavailable(typ string, i int) validationError {
	return validationError{
		Code:    codeProviderUnavailable,
		Message: fmt.Sprintf("no provider registered for resource type %q", typ),
		Path:    fmt.Sprintf("/changes/%d", i),
	}
}

func (s multiSource) checkObserverPreLock(http.ResponseWriter, string) bool {
	// The registry mode has no single capability to check up front; every
	// resource is prechecked individually once the state is read.
	return true
}

func (s multiSource) observerFor(resource planResource, i int) (Observer, *validationError) {
	path := fmt.Sprintf("/resources/%d", i)
	provider, ok := s.providerFor(resource.typ)
	if !ok {
		return nil, &validationError{
			Code:    codeProviderUnavailable,
			Message: fmt.Sprintf("no provider registered for resource type %q", resource.typ),
			Path:    path,
		}
	}
	observer := observerOf(provider)
	if observer == nil {
		return nil, &validationError{
			Code:    codeObserverUnavailable,
			Message: fmt.Sprintf("provider for resource type %q does not implement Observer", resource.typ),
			Path:    path,
		}
	}
	return observer, nil
}

// changeType reports the resource type that routes a planned change: deletes
// route by the before snapshot's type, creates and updates — including
// type-changing updates — by the after snapshot's type.
func changeType(change planChange) string {
	if change.Action == "delete" {
		return change.Before.Type
	}
	return change.After.Type
}
