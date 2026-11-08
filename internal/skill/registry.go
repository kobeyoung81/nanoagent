package skill

import (
	"sync"
)

// Registry wraps Loader with an in-memory cache for skill content.
type Registry struct {
	loader *Loader
	mu     sync.RWMutex
	cache  map[string]string // name -> body
}

// NewRegistry creates a Registry backed by the given Loader.
func NewRegistry(loader *Loader) *Registry {
	return &Registry{
		loader: loader,
		cache:  make(map[string]string),
	}
}

// List returns all known skills (delegates to loader).
func (r *Registry) List() []Skill {
	return r.loader.List()
}

// Get returns the body of a skill by name, using the in-memory cache.
func (r *Registry) Get(name string) string {
	r.mu.RLock()
	if body, ok := r.cache[name]; ok {
		r.mu.RUnlock()
		return body
	}
	r.mu.RUnlock()

	body := r.loader.Load(name)

	r.mu.Lock()
	r.cache[name] = body
	r.mu.Unlock()

	return body
}

// BuildSummary returns an XML summary of all skills (delegates to loader).
func (r *Registry) BuildSummary() string {
	return r.loader.BuildSummary()
}

// GetAlwaysSkills returns names of "always=true" skills that are available.
func (r *Registry) GetAlwaysSkills() []string {
	return r.loader.GetAlwaysSkills()
}

// LoadForContext returns combined content for a list of skill names.
func (r *Registry) LoadForContext(names []string) string {
	return r.loader.LoadForContext(names)
}

// LoadDetail resolves a single skill with its body, base directory and sibling
// files, for on-demand loading by the `skill` tool.
func (r *Registry) LoadDetail(name string) (Detail, bool) {
	return r.loader.LoadDetail(name)
}
