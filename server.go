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
	"sort"
	"strings"
	"time"
)

// Server bundles the proxy state + routing.
type Server struct {
	cfg  *Config
	rt   *Router
	hcli *http.Client
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
	mux.HandleFunc("/v1/messages", s.handleAnthropicMessages)
	mux.HandleFunc("/v1/messages/count_tokens", s.handleAnthropicCountTokens)
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
	clientStream, _ := req["stream"].(bool)
	if prov.MimicOpencode {
		shapeZenRequest(req)
	}
	out, err := json.Marshal(req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "re-encode: " + err.Error()})
		return
	}

	s.forward(w, r, prov, target, out, clientStream)
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
func (s *Server) forward(w http.ResponseWriter, r *http.Request, prov *Provider, modelID string, body []byte, stream bool) {
	url := strings.TrimSuffix(prov.BaseURL, "/") + "/chat/completions"
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// zen requires a streamed upstream even when the caller asked for JSON;
	// the body was reshaped above and the answer gets aggregated below.
	needSSE := stream || prov.MimicOpencode
	dial := s.hcli

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
	if needSSE {
		req.Header.Set("Accept", "text/event-stream")
	}
	// zen's free tier is walled to the official client; mimic its markers.
	if prov.MimicOpencode {
		req.Header.Set("User-Agent", "opencode/1.18.34 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14")
		req.Header.Set("x-opencode-client", "cli")
		req.Header.Set("x-opencode-project", "global")
		req.Header.Set("x-opencode-request", mintOpencodeID("msg"))
		req.Header.Set("x-opencode-session", s.ocSession)
		req.Header.Set("x-opencode-session-id", s.ocSession)
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

	if !needSSE {
		ct := resp.Header.Get("Content-Type")
		if ct == "" {
			ct = "application/json"
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, resp.Body)
		return
	}
	if stream {
		relaySSE(w, resp.Body, ctx)
		return
	}
	aggregateSSE(w, resp.Body, modelID)
}

type streamToolCall struct {
	id, typ, name string
	args          strings.Builder
}

// aggregateSSE collapses a streamed chat.completion.chunk sequence into one
// chat.completion object for clients that asked for stream:false.
func aggregateSSE(w http.ResponseWriter, body io.Reader, modelID string) {
	br := bufio.NewReader(body)
	var content, reasoning strings.Builder
	finish := "stop"
	id := ""
	created := int64(0)
	model := modelID
	usage := map[string]any{}
	calls := map[int]*streamToolCall{}

	for {
		line, err := br.ReadString('\n')
		if s := strings.TrimSpace(line); strings.HasPrefix(s, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(s, "data:"))
			if data == "[DONE]" {
				break
			}
			var ch struct {
				ID      string `json:"id"`
				Created int64  `json:"created"`
				Model   string `json:"model"`
				Choices []struct {
					Delta struct {
						Content          string `json:"content"`
						ReasoningContent string `json:"reasoning_content"`
						ToolCalls        []struct {
							Index    int    `json:"index"`
							ID       string `json:"id"`
							Type     string `json:"type"`
							Function struct {
								Name      string `json:"name"`
								Arguments string `json:"arguments"`
							} `json:"function"`
						} `json:"tool_calls"`
					} `json:"delta"`
					FinishReason *string `json:"finish_reason"`
				} `json:"choices"`
				Usage map[string]any `json:"usage"`
			}
			if json.Unmarshal([]byte(data), &ch) == nil {
				if ch.ID != "" {
					id = ch.ID
				}
				if ch.Created != 0 {
					created = ch.Created
				}
				if ch.Model != "" {
					model = ch.Model
				}
				if ch.Usage != nil {
					usage = ch.Usage
				}
				for _, c := range ch.Choices {
					content.WriteString(c.Delta.Content)
					reasoning.WriteString(c.Delta.ReasoningContent)
					for _, tc := range c.Delta.ToolCalls {
						cur := calls[tc.Index]
						if cur == nil {
							cur = &streamToolCall{}
							calls[tc.Index] = cur
						}
						if tc.ID != "" {
							cur.id = tc.ID
						}
						if tc.Type != "" {
							cur.typ = tc.Type
						}
						cur.name += tc.Function.Name
						cur.args.WriteString(tc.Function.Arguments)
					}
					if c.FinishReason != nil && *c.FinishReason != "" {
						finish = *c.FinishReason
					}
				}
			}
		}
		if err != nil {
			break
		}
	}

	msg := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	if len(calls) > 0 {
		idx := make([]int, 0, len(calls))
		for i := range calls {
			idx = append(idx, i)
		}
		sort.Ints(idx)
		out := make([]any, 0, len(idx))
		for _, i := range idx {
			c := calls[i]
			t := c.typ
			if t == "" {
				t = "function"
			}
			out = append(out, map[string]any{
				"id":   c.id,
				"type": t,
				"function": map[string]any{
					"name":      c.name,
					"arguments": c.args.String(),
				},
			})
		}
		msg["tool_calls"] = out
		if content.Len() == 0 {
			msg["content"] = nil
		}
	}
	if id == "" {
		id = "chatcmpl-" + randHex(20)
	}
	if created == 0 {
		created = unixNow()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
		"usage": usage,
	})
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
