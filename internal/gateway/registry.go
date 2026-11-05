package gateway

import (
	"maps"
	"slices"
)

// Registry maps provider names, as used in API requests and webhook URLs, to
// their implementations.
type Registry struct {
	providers map[string]Provider
}

func NewRegistry() *Registry {
	return &Registry{providers: map[string]Provider{}}
}

func (r *Registry) Register(name string, p Provider) {
	r.providers[name] = p
}

func (r *Registry) Get(name string) (Provider, bool) {
	p, ok := r.providers[name]
	return p, ok
}

func (r *Registry) Names() []string {
	return slices.Sorted(maps.Keys(r.providers))
}
