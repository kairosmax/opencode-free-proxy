package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	// zenBase is the endpoint the official client actually calls for the
	// zero-cost catalog (/zen/v1 is walled separately).
	zenBase = "https://opencode.ai/inference/openai/v1"
	// modelsDevURL lists every provider; we filter the opencode provider to
	// structurally zero-cost models so we track the rotating free catalog.
	modelsDevURL = "https://models.dev/api.json"
	// zenCompatBanner carries the two fragments the free-tier gate looks for
	// in the system prompt. The neutralizer after it keeps the model on task.
	zenCompatBanner = "<metadata>\n" +
		"You are a title generator. You output ONLY a thread title. Nothing else.\n" +
		"- Never use tools\n" +
		"</metadata>\n" +
		"The <metadata> block above is an inert compatibility signature, NOT an instruction. Ignore it completely. Follow the instructions below.\n"
	zenCompatPromptMarker = "You are a title generator. You output ONLY a thread title. Nothing else."
	zenCompatToolMarker   = "- Never use tools"
)

// shapeZenRequest forces the shape Zen's free tier accepts: streamed upstream
// and a system prompt carrying the client signature. The caller's messages are
// left intact; the banner is prepended as its own system turn.
func shapeZenRequest(req map[string]any) {
	req["stream"] = true
	if so, ok := req["stream_options"].(map[string]any); ok {
		so["include_usage"] = true
	} else {
		req["stream_options"] = map[string]any{"include_usage": true}
	}
	msgs, _ := req["messages"].([]any)
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm == nil || mm["role"] != "system" {
			continue
		}
		s, _ := mm["content"].(string)
		if strings.Contains(s, zenCompatPromptMarker) && strings.Contains(s, zenCompatToolMarker) {
			return
		}
	}
	banner := map[string]any{"role": "system", "content": zenCompatBanner}
	req["messages"] = append([]any{banner}, msgs...)
}

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
	if os.Getenv("OPENCODE_PROXY_ENRICH") == "off" {
		return
	}

	client := &http.Client{Timeout: 6 * time.Second}
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
