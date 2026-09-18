package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// Server bundles the proxy state + routing.
type Server struct {
	cfg  *Config
	rt   *Router
	hcli *http.Client
	// mimicTr dials zen with the cloned opencode/Bun TLS fingerprint.
	mimicTr *http.Transport
	// ocSession mirrors the client session marker the official CLI sends to
	// zen; minted once per proxy process with opencode's id scheme.
	ocSession string
}

func newServer(cfg *Config) *Server {
	return &Server{
		cfg:       cfg,
		rt:        newRouter(cfg),
		hcli:      &http.Client{Timeout: 120 * time.Second},
		ocSession: mintOpencodeID("ses"),
		mimicTr: &http.Transport{
			DialTLSContext:    MimicDialContext,
			ForceAttemptHTTP2: false,
			MaxIdleConns:      8,
			IdleConnTimeout:   90 * time.Second,
		},
	}
}

// randHex returns n random hex characters.
func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/chat/completions", s.handleChat)
	return s.withCORS(s.withAuth(mux))
}

func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := s.cfg.Server.Token
		if tok == "" {
			next.ServeHTTP(w, r)
			return
		}
		got := ""
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			got = strings.TrimPrefix(h, "Bearer ")
		} else if h := r.Header.Get("X-API-Key"); h != "" {
			got = h
		} else if q := r.URL.Query().Get("api_key"); q != "" {
			got = q
		}
		if !secureEq(got, tok) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error": {"message": "invalid or missing api key", "type": "auth_error"}}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, X-API-Key, Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":    "opencode-free-proxy",
		"version": "1.0.0",
		"endpoints": []string{
			"GET  /v1/models",
			"POST /v1/chat/completions",
			"GET  /healthz",
		},
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   s.rt.Models(),
	})
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "read body: " + err.Error()})
		return
	}

	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json: " + err.Error()})
		return
	}

	model, _ := req["model"].(string)
	if model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing required field: model"})
		return
	}

	prov, target := s.rt.Resolve(model)
	if prov == nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "no providers configured"})
		return
	}
	if prov.Kind == "serve" {
		s.handleServeChat(w, r, prov, target, body)
		return
	}
	if !prov.Enabled() {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": fmt.Sprintf("provider %q has no api key (set %s)", prov.Name, prov.APIKeyEnv),
		})
		return
	}
	if !s.rt.Allow(prov.Name) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": map[string]any{"message": "provider rate limit exceeded", "type": "rate_limit"},
		})
		return
	}

	// rewrite model to the concrete upstream id
	req["model"] = target
	out, err := json.Marshal(req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "re-encode: " + err.Error()})
		return
	}

	stream, _ := req["stream"].(bool)
	s.forward(w, r, prov, out, stream)
}

// handleServeChat runs the request through the local official opencode server
// (the "serve" lane). It turns the OpenAI shape into session+message calls and
// re-shapes the answer back to OpenAI.
func (s *Server) handleServeChat(w http.ResponseWriter, r *http.Request, prov *Provider, modelID string, rawBody []byte) {
	var req map[string]any
	if err := json.Unmarshal(rawBody, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json: " + err.Error()})
		return
	}

	if err := ensureServe(prov); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "serve lane down: " + err.Error()})
		return
	}
	sid, err := serveCreateSession(r.Context(), prov)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "serve session: " + err.Error()})
		return
	}
	defer serveKillSession(r.Context(), prov, sid)

	prompt := buildPromptText(req["messages"])
	text, usage, err := serveChat(r.Context(), prov, sid, modelID, prompt)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	s.answerOpenAI(w, r, modelID, text, usage, asBool(req["stream"]))
}

// buildPromptText flattens an OpenAI messages array into a single text part.
func buildPromptText(v any) string {
	msgs, _ := v.([]any)
	var sb strings.Builder
	for i, m := range msgs {
		msg, _ := m.(map[string]any)
		role, _ := msg["role"].(string)
		content := contentText(msg["content"])
		if content == "" {
			continue
		}
		if n := len(msgs) - 1; i == n {
			sb.WriteString(content)
			break
		}
		switch role {
		case "system":
			sb.WriteString("System:\n" + content + "\n\n")
		case "assistant":
			sb.WriteString("Assistant:\n" + content + "\n\n")
		default:
			sb.WriteString("User:\n" + content + "\n\n")
		}
	}
	return sb.String()
}

func contentText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, part := range c {
			if pm, ok := part.(map[string]any); ok && pm["type"] == "text" {
				t, _ := pm["text"].(string)
				sb.WriteString(t)
			}
		}
		return sb.String()
	}
	return ""
}

// answerOpenAI writes the OpenAI chat.completion response; streams it as a
// single SSE chunk when the caller asked for SSE.
func (s *Server) answerOpenAI(w http.ResponseWriter, r *http.Request, modelID, text string, usage map[string]int, stream bool) {
	if !stream {
		writeJSON(w, http.StatusOK, map[string]any{
			"id":      "chatcmpl-" + mintOpencodeID("re")[3:20],
			"object":  "chat.completion",
			"created": unixNow(),
			"model":   modelID,
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": text},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{
				"prompt_tokens":     usage["prompt_tokens"],
				"completion_tokens": usage["completion_tokens"],
				"total_tokens":      usage["total_tokens"],
			},
		})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	chunk := map[string]any{
		"id":      "chatcmpl-" + mintOpencodeID("re")[3:20],
		"object":  "chat.completion.chunk",
		"created": unixNow(),
		"model":   modelID,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
	}
	b, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", b)
	fmt.Fprintf(w, "data: [DONE]\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func unixNow() int64 { return time.Now().Unix() }

func asBool(v any) bool {
	b, _ := v.(bool)
	return b
}
func (s *Server) forward(w http.ResponseWriter, r *http.Request, prov *Provider, body []byte, stream bool) {
	url := strings.TrimSuffix(prov.BaseURL, "/") + "/chat/completions"
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	dial := s.hcli
	if prov.MimicOpencode {
		// zen's free tier rejects anything that doesn't speak like the Bun
		// client down at the TLS layer, so we bypass DialTLSContext wholesale.
		dial = &http.Client{Transport: s.mimicTr, Timeout: s.hcli.Timeout}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if prov.key != "" {
		req.Header.Set("Authorization", "Bearer "+prov.key)
	}
	req.Header.Set("Accept", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	// zen's free tier is walled to the official client; mimic its markers.
	if prov.MimicOpencode {
		req.Header.Set("User-Agent", "opencode/1.18.18 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14")
		req.Header.Set("x-opencode-client", "cli")
		req.Header.Set("x-opencode-project", "global")
		req.Header.Set("x-opencode-request", mintOpencodeID("msg"))
		req.Header.Set("x-opencode-session", s.ocSession)
	}
	for k, v := range prov.Headers {
		req.Header.Set(k, v)
	}

	hcli := dial
	if prov.TimeoutMs > 0 && !stream {
		hcli = &http.Client{Transport: hcli.Transport, Timeout: time.Duration(prov.TimeoutMs) * time.Millisecond}
	}

	resp, err := hcli.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "upstream: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	// pass upstream error responses through untouched
	if resp.StatusCode != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	if stream {
		relaySSE(w, resp.Body, ctx)
		return
	}

	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, resp.Body)
}

// relaySSE copies upstream server-sent-events to the client line by line.
func relaySSE(w http.ResponseWriter, body io.ReadCloser, ctx context.Context) {
	fl, ok := w.(http.Flusher)
	if !ok {
		io.Copy(w, body)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	done := make(chan struct{})
	go func() {
		defer close(done)
		br := bufio.NewReader(body)
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				if _, werr := w.Write(line); werr != nil {
					return
				}
				fl.Flush()
			}
			if err != nil {
				return
			}
		}
	}()

	select {
	case <-done:
	case <-ctx.Done():
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// secureEq is a constant-time compare for auth tokens.
func secureEq(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// silentLogger keeps the proxy quiet but still surfaces fatal startup errors.
var _ = log.Printf
