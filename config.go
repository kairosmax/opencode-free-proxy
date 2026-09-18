package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Config is the root config file shape.
type Config struct {
	Server    ServerConfig     `json:"server"`
	Providers []*Provider      `json:"providers"`
	Aliases   map[string]Alias `json:"aliases"`
	Default   string           `json:"default_provider"`
}

// ServerConfig controls the HTTP listener.
type ServerConfig struct {
	Listen string `json:"listen"`
	Token  string `json:"token"`
}

// Provider is a single upstream chat-completions backend.
type Provider struct {
	Name          string            `json:"name"`
	BaseURL       string            `json:"base_url"`
	Kind          string            `json:"kind"` // "openai" (default) | "serve"
	APIKeyEnv     string            `json:"api_key_env"`
	APIKey        string            `json:"api_key"`
	KeyRequired   bool              `json:"key_required"`
	MimicOpencode bool              `json:"mimic_opencode"`
	Headers       map[string]string `json:"headers"`
	Models        []string          `json:"models"`
	DefaultModel  string            `json:"default_model"`
	Agent         string            `json:"agent"`
	RPS           float64           `json:"rps"`
	TimeoutMs     int               `json:"timeout_ms"`

	key string
}

// Alias maps a friendly model id to a concrete provider + upstream model.
type Alias struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// DefaultConfig returns a working starter config with the big free-tier
// backends pre-wired. Keys are consumed from env vars when present.
func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Listen: "127.0.0.1:8080",
		},
		Providers: []*Provider{
			serveProvider(),
			zenProvider(),
			{
				Name:      "openrouter",
				BaseURL:   "https://openrouter.ai/api/v1",
				APIKeyEnv: "OPENROUTER_API_KEY",
				Models: []string{
					"deepseek/deepseek-r1:free",
					"deepseek/deepseek-chat:free",
					"meta-llama/llama-3.3-70b-instruct:free",
					"qwen/qwen-2.5-72b-instruct:free",
					"microsoft/phi-4:free",
					"z-ai/glm-4-9b-chat:free",
				},
				DefaultModel: "deepseek/deepseek-r1:free",
				RPS:          10,
				TimeoutMs:    120000,
			},
			{
				Name:      "groq",
				BaseURL:   "https://api.groq.com/openai/v1",
				APIKeyEnv: "GROQ_API_KEY",
				Models: []string{
					"llama-3.3-70b-versatile",
					"llama-3.1-8b-instant",
					"meta-llama/llama-4-scout-17b-16e-instruct",
				},
				DefaultModel: "llama-3.3-70b-versatile",
				RPS:          5,
				TimeoutMs:    60000,
			},
			{
				Name:      "gemini",
				BaseURL:   "https://generativelanguage.googleapis.com/v1beta/openai",
				APIKeyEnv: "GEMINI_API_KEY",
				Models: []string{
					"gemini-2.5-flash",
					"gemini-2.0-flash",
					"gemini-2.0-flash-lite",
				},
				DefaultModel: "gemini-2.5-flash",
				RPS:          15,
				TimeoutMs:    120000,
			},
		},
		Aliases: map[string]Alias{
			"pickle":     {Provider: "serve", Model: "big-pickle"},
			"big-pickle": {Provider: "serve", Model: "big-pickle"},
			"free":       {Provider: "serve", Model: "big-pickle"},
			"zen":        {Provider: "zen", Model: "big-pickle"},
			"r1":         {Provider: "openrouter", Model: "deepseek/deepseek-r1:free"},
			"deepseek":   {Provider: "openrouter", Model: "deepseek/deepseek-chat:free"},
			"llama":      {Provider: "openrouter", Model: "meta-llama/llama-3.3-70b-instruct:free"},
			"llama-g":    {Provider: "groq", Model: "llama-3.3-70b-versatile"},
			"flash":      {Provider: "gemini", Model: "gemini-2.5-flash"},
			"flash-lite": {Provider: "gemini", Model: "gemini-2.0-flash-lite"},
		},
		Default: "openrouter",
	}
}

// LoadConfig reads config.json if present, fills gaps from defaults, and
// resolves API keys from file or env. Returns a config ready to serve.
func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("read config: %w", err)
			}
		} else {
			var custom Config
			if err := json.Unmarshal(data, &custom); err != nil {
				return nil, fmt.Errorf("parse config %s: %w", path, err)
			}
			cfg = MergeConfigs(cfg, &custom)
		}
	}
	for _, p := range cfg.Providers {
		p.key = os.Getenv(p.APIKeyEnv)
		if p.key == "" {
			p.key = p.APIKey
		}
		if p.TimeoutMs == 0 {
			p.TimeoutMs = 120000
		}
	}
	if cfg.Default == "" && len(cfg.Providers) > 0 {
		cfg.Default = cfg.Providers[0].Name
	}
	return cfg, nil
}

// MergeConfigs overlays user fields on top of default ones so partial configs
// don't wipe the working defaults.
func MergeConfigs(base, over *Config) *Config {
	if over.Server.Listen != "" {
		base.Server.Listen = over.Server.Listen
	}
	if over.Server.Token != "" {
		base.Server.Token = over.Server.Token
	}
	if len(over.Providers) > 0 {
		base.Providers = over.Providers
		base.Default = over.Default
	}
	if len(over.Aliases) > 0 {
		if base.Aliases == nil {
			base.Aliases = map[string]Alias{}
		}
		for k, v := range over.Aliases {
			base.Aliases[k] = v
		}
	}
	if over.Default != "" {
		base.Default = over.Default
	}
	return base
}

// Enabled reports whether a provider can be used: key-required providers
// need a key, keyless ones (zen) are always ready.
func (p *Provider) Enabled() bool {
	if !p.KeyRequired {
		return true
	}
	return p.key != ""
}

// Key returns the resolved API key (empty for keyless providers).
func (p *Provider) Key() string { return p.key }

// proxies URL-part of a provider id (the part before the first slash).
func providerFromID(model string) string {
	if i := strings.IndexByte(model, '/'); i > 0 {
		return model[:i]
	}
	return ""
}
