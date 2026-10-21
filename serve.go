package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// serve.go — "serve" lane: drives the local official opencode server
// (`opencode serve`) so zen free models flow through the file-descriptor the
// vendor actually signed. The wire path stays 100% opencode.

const serveAgentDefault = "plan"

// logFile where the autostarted serve drops its noise.
func serveLogPath() string {
	return os.TempDir() + "/opencode-serve.log"
}

// proxyDataHome keeps the proxy's `opencode serve` in its own XDG_DATA_HOME so
// its sessions never land in the same store the interactive TUI is using.
func proxyDataHome() string {
	if v := os.Getenv("OPENCODE_PROXY_DATA_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".local", "share", "opencode-proxy-svc")
}

// seedAuth copies the existing opencode credentials into the isolated data
// home once, so the dedicated serve keeps zen access without a fresh login.
func seedAuth(dataHome string) {
	if dataHome == "" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return
	}
	src := filepath.Join(home, ".local", "share", "opencode", "auth.json")
	dstDir := filepath.Join(dataHome, "opencode")
	dst := filepath.Join(dstDir, "auth.json")
	if _, err := os.Stat(dst); err == nil {
		return
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		return
	}
	if err := os.MkdirAll(dstDir, 0700); err != nil {
		return
	}
	_ = os.WriteFile(dst, raw, 0600)
}

func serveHealthURL(base string) string {
	return strings.TrimSuffix(base, "/") + "/global/health"
}

// ensureServe up-checks the health endpoint and autostarts `opencode serve`
// when the lane is configured but down.
func ensureServe(p *Provider) error {
	if p.BaseURL == "" {
		return fmt.Errorf("serve provider missing base_url")
	}
	if probeServe(p) {
		return nil
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil {
		return err
	}
	_, port, _ := net.SplitHostPort(u.Host)
	host, _, _ := net.SplitHostPort(u.Host)
	if host == "" {
		host = "127.0.0.1"
	}
	if port == "" {
		port = "8377"
	}

	lf, err := os.OpenFile(serveLogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	cmd := exec.Command("opencode", "serve", "--hostname", host, "--port", port)
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.Dir = os.TempDir()
	dataHome := proxyDataHome()
	seedAuth(dataHome)
	if dataHome != "" {
		cmd.Env = append(os.Environ(), "XDG_DATA_HOME="+dataHome)
	}
	// detach into its own process group so the server outlives us
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("autostart serve: %w", err)
	}
	go func() { _ = cmd.Wait() }()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if probeServe(p) {
			return nil
		}
		time.Sleep(600 * time.Millisecond)
	}
	return fmt.Errorf("serve lane never came up at %s (log: %s)", p.BaseURL, serveLogPath())
}

func probeServe(p *Provider) bool {
	cl := &http.Client{Timeout: 4 * time.Second}
	r, err := cl.Get(serveHealthURL(p.BaseURL))
	if err != nil {
		// the endpoint may 404 on older builds; any TCP answer means alive
		if !isConnRefused(err) {
			return true
		}
		return false
	}
	defer r.Body.Close()
	return true
}

func isConnRefused(err error) bool {
	return err != nil && strings.Contains(err.Error(), "refused")
}

// servePrompt asks the local opencode server for one chat turn.
type servePromptIn struct {
	SessionID string
	ModelID   string
	Text      string
	Agent     string
}

type serveMsgOut struct {
	Info struct {
		Role   string `json:"role"`
		Tokens struct {
			Total     int `json:"total"`
			Input     int `json:"input"`
			Output    int `json:"output"`
			Reasoning int `json:"reasoning"`
		} `json:"tokens"`
	} `json:"info"`
	Parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		Tool string `json:"tool"`
	} `json:"parts"`
}

// serveChat runs one chat turn over the local opencode server.
// Usage for LO: "give me a text part and I return what the assistant said".
func serveChat(ctx context.Context, p *Provider, sid string, modelID, text string) (string, map[string]int, error) {
	payload := map[string]any{
		"model": map[string]string{"providerID": "opencode", "modelID": modelID},
		"agent": serveAgent(p, serveAgentDefault),
		"parts": []map[string]any{{"type": "text", "text": text}},
	}
	body, _ := json.Marshal(payload)
	ep := strings.TrimSuffix(p.BaseURL, "/") + "/session/" + sid + "/message"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep, bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := serveClient().Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("serve upstream: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return "", nil, fmt.Errorf("serve upstream %d: %s", resp.StatusCode, truncate(string(raw), 400))
	}
	var out serveMsgOut
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", nil, fmt.Errorf("decode serve reply: %w", err)
	}
	var sb strings.Builder
	for _, part := range out.Parts {
		if part.Type == "text" {
			sb.WriteString(part.Text)
		}
	}
	usage := map[string]int{
		"prompt_tokens":     out.Info.Tokens.Input,
		"completion_tokens": out.Info.Tokens.Output,
		"total_tokens":      out.Info.Tokens.Total,
	}
	return sb.String(), usage, nil
}

func serveAgent(p *Provider, def string) string {
	// future config knob; per now stuck on the read-only "plan"
	if p.Agent != "" {
		return p.Agent
	}
	return def
}

func serveClient() *http.Client {
	return &http.Client{Timeout: 300 * time.Second}
}

// serveCreateSession asks the server for a fresh session to park one probe in.
func serveCreateSession(ctx context.Context, p *Provider) (string, error) {
	ep := strings.TrimSuffix(p.BaseURL, "/") + "/session"
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ep, bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	resp, err := serveClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("create session %d: %s", resp.StatusCode, truncate(string(b), 200))
	}
	var doc struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", err
	}
	return doc.ID, nil
}

// serveKillSession politely closes the probe session afterwards.
func serveKillSession(ctx context.Context, p *Provider, sid string) {
	ep := strings.TrimSuffix(p.BaseURL, "/") + "/session/" + sid // kill
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, ep, nil)
	resp, err := serveClient().Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
