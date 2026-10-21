package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// runSelfTest probes both lanes: the direct-zen clone and the serve lane.
// Usage: ./opencode-api -selftest
func runSelfTest(cfg *Config) int {
	rc := 0

	// Lane 1: direct zen (clone fingerprint). Expected to wobble with gate updates.
	if code := probeZen(); code == 0 {
		fmt.Println("ZEN direct:   ✅ sirve")
	} else {
		fmt.Println("ZEN direct:   ❌ movida — la receta de la muralla dio la vuelta otra vez")
		rc = 2
	}

	// Lane 2: serve lane (official client). Should always be green.
	if err := probeServeLane(cfg); err == nil {
		fmt.Println("SERVE (opencode local): ✅ sirve — carril seguro activo")
	} else {
		fmt.Println("SERVE (opencode local): ❌", err)
		rc = 2
	}
	return rc
}

func probeZen() int {
	payload := map[string]any{
		"model":          "big-pickle",
		"stream":         true,
		"max_tokens":     8,
		"stream_options": map[string]any{"include_usage": true},
		"messages": []any{
			map[string]any{"role": "system", "content": zenCompatBanner + "You are a helpful assistant."},
			map[string]any{"role": "user", "content": "Reply with exactly: selftest-ok"},
		},
	}
	body, _ := json.Marshal(payload)
	cl := &http.Client{Timeout: 60 * time.Second}
	req, err := http.NewRequest(http.MethodPost, zenBase+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		fmt.Println("selftest: build request:", err)
		return 1
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer public")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", "opencode/1.18.34 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14")
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-project", "global")
	req.Header.Set("x-opencode-request", mintOpencodeID("msg"))
	req.Header.Set("x-opencode-session", mintOpencodeID("ses"))
	req.Header.Set("x-opencode-session-id", req.Header.Get("x-opencode-session"))

	resp, err := cl.Do(req)
	if err != nil {
		fmt.Println("selftest: network:", err)
		return 1
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	fmt.Fprintf(os.Stdout, "  → zen status %d: %.160s\n", resp.StatusCode, string(raw))
	if resp.StatusCode == http.StatusOK && strings.Contains(string(raw), `"content"`) {
		return 0
	}
	return 2
}

func probeServeLane(cfg *Config) error {
	for _, p := range cfg.Providers {
		if p.Kind != "serve" {
			continue
		}
		if err := ensureServe(p); err != nil {
			return err
		}
		ctx := context.Background()
		sid, err := serveCreateSession(ctx, p)
		if err != nil {
			return err
		}
		defer serveKillSession(ctx, p, sid)
		txt, _, err := serveChat(ctx, p, sid, "big-pickle", "Reply exactly: serve-ok")
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "  → serve respondió: %.80s\n", txt)
		return nil
	}
	return fmt.Errorf("no serve provider configurado")
}
