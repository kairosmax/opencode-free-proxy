package main

import (
	"strings"
	"sync"
	"time"
)

// Router resolves a requested model id to a concrete provider upstream.
type Router struct {
	providers   map[string]*Provider
	aliases     map[string]Alias
	defaultName string
	limits      map[string]*bucket
	mu          sync.Mutex
}

// bucket is a tiny token bucket for free-tier rate limits.
type bucket struct {
	rps    float64
	tokens float64
	last   time.Time
}

func newRouter(cfg *Config) *Router {
	r := &Router{
		providers:   make(map[string]*Provider, len(cfg.Providers)),
		aliases:     cfg.Aliases,
		defaultName: cfg.Default,
		limits:      make(map[string]*bucket, len(cfg.Providers)),
	}
	for _, p := range cfg.Providers {
		r.providers[p.Name] = p
		if p.RPS > 0 {
			r.limits[p.Name] = &bucket{rps: p.RPS, tokens: p.RPS, last: time.Now()}
		}
	}
	return r
}

// Resolve picks the provider + upstream model for a client-sent model id.
// Priority: explicit alias -> provider/model form -> provider member list ->
// default provider passthrough.
func (r *Router) Resolve(model string) (*Provider, string) {
	if model == "" {
		model = r.defaultName
	}

	// exact alias lookup wins
	if a, ok := r.aliases[model]; ok {
		if p := r.providers[a.Provider]; p != nil {
			return p, a.Model
		}
	}

	// try "provider/model-id" form
	if name := providerFromID(model); name != "" {
		if p, ok := r.providers[name]; ok {
			return p, model[len(name)+1:]
		}
		// "bundle/openrouter/..." style prefixes; strip leading tags
		if p, ok := r.providers[strings.ToLower(name)]; ok {
			return p, model[len(name)+1:]
		}
	}

	// model listed under a specific provider
	for _, p := range r.providers {
		for _, m := range p.Models {
			if m == model {
				return p, m
			}
		}
	}

	// fall back to default provider, pass model through untouched
	def := r.providers[r.defaultName]
	if def == nil || !def.Enabled() {
		for _, p := range r.providers {
			if p.Enabled() {
				def = p
				break
			}
		}
	}
	if def == nil {
		return nil, model
	}
	if def.DefaultModel != "" {
		return def, def.DefaultModel
	}
	return def, model
}

// Allow throttles a provider against its token bucket. Returns false when the
// client should be 429'd.
func (r *Router) Allow(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.limits[name]
	if !ok {
		return true
	}
	now := time.Now()
	d := now.Sub(b.last).Seconds()
	b.tokens += d * b.rps
	if b.tokens > b.rps {
		b.tokens = b.rps
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Models returns the catalog of ids clients may request, OpenAI shape.
func (r *Router) Models() []map[string]any {
	out := []map[string]any{}
	for alias, a := range r.aliases {
		if p, ok := r.providers[a.Provider]; ok {
			if !p.Enabled() {
				continue
			}
			out = append(out, map[string]any{
				"id":       alias,
				"object":   "model",
				"owned_by": a.Provider,
				"created":  0,
			})
		}
	}
	for _, p := range r.providers {
		if !p.Enabled() {
			continue
		}
		for _, m := range p.Models {
			out = append(out, map[string]any{
				"id":       p.Name + "/" + m,
				"object":   "model",
				"owned_by": p.Name,
				"created":  0,
			})
		}
	}
	return out
}
