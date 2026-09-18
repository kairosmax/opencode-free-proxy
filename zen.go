package main

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"time"
)

const (
	// zenBase is opencode's own OpenAI-compatible gateway. The zero-cost
	// models (big-pickle & friends) need no API key at all.
	zenBase = "https://opencode.ai/zen/v1"
	// modelsDevURL lists every provider; we filter the opencode provider to
	// structurally zero-cost models so we track the rotating free catalog.
	modelsDevURL = "https://models.dev/api.json"
)

// zenProvider returns the default zero-cost opencode provider definition.
// Zen's free tier is gated to the official client, so the proxy speaks the
// same dialect: Bearer public + x-opencode-* client markers.
func zenProvider() *Provider {
	return &Provider{
		Name:          "zen",
		BaseURL:       zenBase,
		APIKey:        "public",
		KeyRequired:   false,
		MimicOpencode: true,
		Models: []string{
			"big-pickle",
			"minimax-m2.1-free",
			"deepseek-v4-flash-free",
			"nemotron-3-ultra-free",
		},
		DefaultModel: "big-pickle",
		RPS:          3,
		TimeoutMs:    180000,
	}
}

// serveProvider tunnels requests through the local official opencode server
// (`opencode serve`), which always clears Zen's client-side checks. It's the
// unbreakable lane: vendor-proof since the vendor drives it.
func serveProvider() *Provider {
	return &Provider{
		Name:         "serve",
		BaseURL:      "http://127.0.0.1:8377",
		Kind:         "serve",
		KeyRequired:  false,
		Agent:        "general",
		Models:       []string{"big-pickle"},
		DefaultModel: "big-pickle",
		RPS:          1,
		TimeoutMs:    300000,
	}
}

// EnrichZen keeps the zen provider's model list in sync with models.dev so
// the free catalog rotates without anyone editing config. Non-fatal: if the
// registry can't be reached the static defaults are fine.
func EnrichZen(cfg *Config) {
	zen := findProvider(cfg, "zen")
	if zen == nil {
		cfg.Providers = append(cfg.Providers, zenProvider())
		zen = findProvider(cfg, "zen")
	}

	client := &http.Client{Timeout: 12 * time.Second}
	resp, err := client.Get(modelsDevURL)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return
	}

	var reg map[string]struct {
		Models map[string]struct {
			Cost map[string]any `json:"cost"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &reg); err != nil {
		return
	}
	oc, ok := reg["opencode"]
	if !ok {
		return
	}

	free := []string{}
	for id, m := range oc.Models {
		c := m.Cost
		if c == nil {
			continue
		}
		if num(c["input"]) == 0 && num(c["output"]) == 0 {
			free = append(free, id)
		}
	}
	if len(free) == 0 {
		return
	}
	sort.Strings(free)
	zen.Models = free
	if zen.DefaultModel == "" || !contains(free, zen.DefaultModel) {
		zen.DefaultModel = "big-pickle"
	}
	// serve lane rides the same rotating catalog
	if sp := findProvider(cfg, "serve"); sp != nil {
		sp.Models = free
		if sp.DefaultModel == "" || !contains(free, sp.DefaultModel) {
			sp.DefaultModel = "big-pickle"
		}
	}
	_ = cfg
}

// num coerces a JSON number field to float64 (handles non-numeric cost fields
// like the array-valued tiers gracefully).
func num(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return -1
}

func findProvider(cfg *Config, name string) *Provider {
	for _, p := range cfg.Providers {
		if p.Name == name {
			return p
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
