package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// anthropic.go exposes Anthropic's Messages API (/v1/messages) so Claude Code
// can ride the same free zen/serve lanes the OpenAI endpoint uses. The wire
// translation is intentionally lossless for the parts Claude Code needs:
// system prompt, text, images, tool_use / tool_result and SSE streaming.

func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<20))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid json: "+err.Error())
		return
	}
	model, _ := req["model"].(string)
	if model == "" {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "missing required field: model")
		return
	}
	debug := os.Getenv("OPENCODE_PROXY_DEBUG") != ""
	var seq uint64
	if debug {
		seq = anthSeq.Add(1)
		_ = os.WriteFile(fmt.Sprintf("%s/opencode-anth-%d-in.json", os.TempDir(), seq), body, 0600)
		fmt.Fprintf(os.Stderr, "anth[%d] model=%s bytes=%d stream=%v\n", seq, model, len(body), req["stream"])
	}
	prov, target := s.rt.Resolve(model)
	if prov == nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "no providers configured")
		return
	}
	if !prov.Enabled() {
		writeAnthropicError(w, http.StatusBadGateway, "api_error",
			fmt.Sprintf("provider %q has no api key (set %s)", prov.Name, prov.APIKeyEnv))
		return
	}
	if !s.rt.Allow(prov.Name) {
		writeAnthropicError(w, http.StatusTooManyRequests, "rate_limit_error", "provider rate limit exceeded")
		return
	}

	if prov.Kind == "serve" {
		s.handleAnthropicServe(w, r, prov, target, req, model)
		return
	}

	oai, err := anthropicToOpenAI(req, target)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	// The free lane only answers with stream:true; we translate or aggregate.
	oai["stream"] = true
	oai["stream_options"] = map[string]any{"include_usage": true}
	if prov.MimicOpencode {
		shapeZenRequest(oai)
		if os.Getenv("OPENCODE_PROXY_DEBUG") != "" {
			if dbg, err := json.MarshalIndent(oai, "", "  "); err == nil {
				_ = os.WriteFile(fmt.Sprintf("%s/opencode-anth-%d-zen.json", os.TempDir(), seq), dbg, 0600)
			}
		}
	}
	applyInstructions(oai, s.instructionsFor(r))
	out, err := json.Marshal(oai)
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "re-encode: "+err.Error())
		return
	}

	clientStream, _ := req["stream"].(bool)
	emitThink := thinkingEnabled() && thinkingRequested(req)
	resp, err := s.anthropicUpstream(r.Context(), prov, out, clientStream)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		if debug {
			fmt.Fprintf(os.Stderr, "anth[%d] upstream %d model=%s: %.300s\n", seq, resp.StatusCode, target, string(raw))
		}
		writeAnthropicError(w, resp.StatusCode, "api_error", fmt.Sprintf("upstream %d: %s", resp.StatusCode, truncate(string(raw), 600)))
		return
	}

	estIn := anthropicEstimateTokens(body)
	if clientStream {
		relayAnthropicStream(w, resp.Body, model, estIn, emitThink)
		return
	}
	writeAnthropicAggregate(w, resp.Body, model, estIn, emitThink)
}

func (s *Server) handleAnthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<20))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": anthropicEstimateTokens(body)})
}

// anthropicUpstream sends the translated body. Streaming requests get a client
// without an overall deadline so long Claude Code turns are not cut at 120s.
func (s *Server) anthropicUpstream(ctx context.Context, prov *Provider, body []byte, stream bool) (*http.Response, error) {
	url := strings.TrimSuffix(prov.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if prov.Key() != "" {
		req.Header.Set("Authorization", "Bearer "+prov.Key())
	}
	if prov.MimicOpencode {
		req.Header.Set("User-Agent", "opencode/1.18.10 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14")
		req.Header.Set("x-opencode-client", "cli")
		req.Header.Set("x-opencode-project", "global")
		req.Header.Set("x-opencode-request", mintOpencodeID("msg"))
		req.Header.Set("x-opencode-session", s.ocSession)
		req.Header.Set("x-opencode-session-id", s.ocSession)
	}
	for k, v := range prov.Headers {
		req.Header.Set(k, v)
	}
	tr := s.hcli.Transport
	if prov.MimicOpencode {
		tr = zenTransport
	}
	cl := &http.Client{Transport: tr}
	if stream {
		cl = &http.Client{Transport: tr}
	} else if prov.TimeoutMs > 0 {
		cl = &http.Client{Transport: tr, Timeout: time.Duration(prov.TimeoutMs) * time.Millisecond}
	} else {
		cl.Timeout = s.hcli.Timeout
	}
	return cl.Do(req)
}

// anthropicToOpenAI maps the Messages request onto chat.completions.
func anthropicToOpenAI(req map[string]any, target string) (map[string]any, error) {
	out := map[string]any{"model": target}

	msgs := []any{}
	if sys := anthropicText(req["system"]); sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}

	raw, _ := req["messages"].([]any)
	for _, m := range raw {
		mm, _ := m.(map[string]any)
		if mm == nil {
			continue
		}
		role, _ := mm["role"].(string)
		switch c := mm["content"].(type) {
		case string:
			msgs = append(msgs, map[string]any{"role": role, "content": c})
		case []any:
			if role == "assistant" {
				text := strings.Builder{}
				calls := []any{}
				for _, b := range c {
					bb, _ := b.(map[string]any)
					if bb == nil {
						continue
					}
					switch bb["type"] {
					case "text":
						text.WriteString(str(bb["text"]))
					case "tool_use":
						args, _ := json.Marshal(bb["input"])
						calls = append(calls, map[string]any{
							"id":   str(bb["id"]),
							"type": "function",
							"function": map[string]any{
								"name":      str(bb["name"]),
								"arguments": string(args),
							},
						})
					}
				}
				msg := map[string]any{"role": "assistant"}
				if text.Len() > 0 {
					msg["content"] = text.String()
				} else if len(calls) > 0 {
					msg["content"] = nil
				} else {
					msg["content"] = ""
				}
				if len(calls) > 0 {
					msg["tool_calls"] = calls
				}
				msgs = append(msgs, msg)
				continue
			}

			parts := []any{}
			toolMsgs := []any{}
			for _, b := range c {
				bb, _ := b.(map[string]any)
				if bb == nil {
					continue
				}
				switch bb["type"] {
				case "text":
					if t := str(bb["text"]); t != "" {
						parts = append(parts, map[string]any{"type": "text", "text": t})
					}
				case "image":
					if u := anthropicImageURL(bb); u != "" {
						parts = append(parts, map[string]any{
							"type":      "image_url",
							"image_url": map[string]any{"url": u},
						})
					}
				case "tool_result":
					content := anthropicToolResultText(bb["content"])
					if errFlag, _ := bb["is_error"].(bool); errFlag {
						content = "Error: " + content
					}
					toolMsgs = append(toolMsgs, map[string]any{
						"role":         "tool",
						"tool_call_id": str(bb["tool_use_id"]),
						"content":      content,
					})
				}
			}
			msgs = append(msgs, toolMsgs...)
			if len(parts) == 1 {
				if pm, _ := parts[0].(map[string]any); pm != nil && pm["type"] == "text" {
					msgs = append(msgs, map[string]any{"role": "user", "content": pm["text"]})
					continue
				}
			}
			if len(parts) > 0 {
				msgs = append(msgs, map[string]any{"role": "user", "content": parts})
			}
		}
	}
	out["messages"] = msgs

	if v, ok := req["max_tokens"]; ok {
		out["max_tokens"] = v
	}
	for _, k := range []string{"temperature", "top_p"} {
		if v, ok := req[k]; ok {
			out[k] = v
		}
	}
	if v, ok := req["stop_sequences"]; ok {
		out["stop"] = v
	}
	if tools := buildAnthropicTools(req["tools"]); len(tools) > 0 {
		out["tools"] = tools
		if tc, ok := mapAnthropicToolChoice(req["tool_choice"]); ok {
			out["tool_choice"] = tc
		}
	}
	return out, nil
}

func buildAnthropicTools(v any) []any {
	raw, _ := v.([]any)
	out := []any{}
	for _, t := range raw {
		tt, _ := t.(map[string]any)
		if tt == nil {
			continue
		}
		name := str(tt["name"])
		if name == "" {
			continue
		}
		fn := map[string]any{"name": name}
		if d := str(tt["description"]); d != "" {
			fn["description"] = d
		}
		if in, ok := tt["input_schema"]; ok && in != nil {
			fn["parameters"] = in
		} else {
			fn["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

func mapAnthropicToolChoice(v any) (any, bool) {
	tc, _ := v.(map[string]any)
	if tc == nil {
		return nil, false
	}
	switch str(tc["type"]) {
	case "auto":
		return "auto", true
	case "any":
		return "required", true
	case "none":
		return "none", true
	case "tool":
		name := str(tc["name"])
		if name == "" {
			return nil, false
		}
		return map[string]any{"type": "function", "function": map[string]any{"name": name}}, true
	}
	return nil, false
}

func anthropicText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var sb strings.Builder
		for _, b := range t {
			bb, _ := b.(map[string]any)
			if bb != nil && str(bb["type"]) == "text" {
				sb.WriteString(str(bb["text"]))
			}
		}
		return sb.String()
	}
	return ""
}

func anthropicToolResultText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var sb strings.Builder
		for _, b := range t {
			bb, _ := b.(map[string]any)
			if bb == nil {
				continue
			}
			switch str(bb["type"]) {
			case "text":
				sb.WriteString(str(bb["text"]))
			case "image":
				sb.WriteString("[image]")
			}
		}
		return sb.String()
	}
	return ""
}

func anthropicImageURL(block map[string]any) string {
	src, _ := block["source"].(map[string]any)
	if src == nil {
		return ""
	}
	switch str(src["type"]) {
	case "base64":
		mt := str(src["media_type"])
		if mt == "" {
			mt = "image/png"
		}
		return "data:" + mt + ";base64," + str(src["data"])
	case "url":
		return str(src["url"])
	}
	return ""
}

func anthropicEstimateTokens(body []byte) int {
	// Rough 4-chars-per-token accounting; only used for client bookkeeping.
	n := len(body) / 4
	if n < 1 {
		n = 1
	}
	return n
}

func writeAnthropicError(w http.ResponseWriter, code int, typ, msg string) {
	writeJSON(w, code, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": msg},
	})
}

// ---- upstream stream state ----

type oaiToolCall struct {
	id, typ, name string
	args          strings.Builder
}

type anthropicStreamer struct {
	w         http.ResponseWriter
	fl        http.Flusher
	model     string
	blocks    int
	textIdx   int
	textOpen  bool
	thinkIdx  int
	thinkOpen bool
	emitThink bool
	calls     map[int]*oaiToolCall
	order     []int
	finish    string
	usage     map[string]any
}

func relayAnthropicStream(w http.ResponseWriter, body io.ReadCloser, model string, estIn int, emitThink bool) {
	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	st := &anthropicStreamer{
		w:         w,
		fl:        fl,
		model:     model,
		textIdx:   -1,
		thinkIdx:  -1,
		emitThink: emitThink,
		calls:     map[int]*oaiToolCall{},
	}
	st.send("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            mintOpencodeID("msg"),
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": estIn, "output_tokens": 1},
		},
	})

	br := bufio.NewReader(body)
	for {
		line, err := br.ReadString('\n')
		if s := strings.TrimSpace(line); strings.HasPrefix(s, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(s, "data:"))
			if data == "[DONE]" {
				break
			}
			st.consume([]byte(data))
		}
		if err != nil {
			break
		}
	}
	st.closeThinking()
	st.closeText()
	for _, idx := range st.order {
		c := st.calls[idx]
		if c == nil {
			continue
		}
		id := c.id
		if id == "" {
			id = mintOpencodeID("toolu")
		}
		blockIdx := st.startBlock(map[string]any{"type": "tool_use", "id": id, "name": clientToolName(c.name), "input": map[string]any{}})
		args := c.args.String()
		if args == "" {
			args = "{}"
		}
		st.send("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": blockIdx,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
		})
		st.send("content_block_stop", map[string]any{"type": "content_block_stop", "index": blockIdx})
	}
	reason := anthropicStopReason(st.finish, len(st.calls) > 0)
	outTokens := estimateOutputTokens(st)
	st.send("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": reason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": outTokens},
	})
	st.send("message_stop", map[string]any{"type": "message_stop"})
}

func (st *anthropicStreamer) send(event string, payload any) {
	b, _ := json.Marshal(payload)
	fmt.Fprintf(st.w, "event: %s\ndata: %s\n\n", event, b)
	if st.fl != nil {
		st.fl.Flush()
	}
}

func (st *anthropicStreamer) consume(data []byte) {
	var ch struct {
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
	if err := json.Unmarshal(data, &ch); err != nil {
		return
	}
	if ch.Usage != nil {
		st.usage = ch.Usage
	}
	for _, c := range ch.Choices {
		if r := c.Delta.ReasoningContent; r != "" && st.emitThink && st.canOpenThinking() {
			if !st.thinkOpen {
				st.thinkIdx = st.startBlock(map[string]any{"type": "thinking", "thinking": "", "signature": ""})
				st.thinkOpen = true
			}
			st.send("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": st.thinkIdx,
				"delta": map[string]any{"type": "thinking_delta", "thinking": r},
			})
		}
		if c.Delta.Content != "" {
			st.closeThinking()
			if !st.textOpen {
				st.textIdx = st.startBlock(map[string]any{"type": "text", "text": ""})
				st.textOpen = true
			}
			st.send("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": st.textIdx,
				"delta": map[string]any{"type": "text_delta", "text": c.Delta.Content},
			})
		}
		for _, tc := range c.Delta.ToolCalls {
			cur := st.calls[tc.Index]
			if cur == nil {
				cur = &oaiToolCall{}
				st.calls[tc.Index] = cur
				st.order = append(st.order, tc.Index)
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
			st.finish = *c.FinishReason
		}
	}
}

// startBlock emite el content_block_start y devuelve el índice asignado,
// avanzando el contador para que el llamador use el mismo índice en los
// deltas y en el stop.
func (st *anthropicStreamer) startBlock(block map[string]any) int {
	idx := st.blocks
	st.send("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         idx,
		"content_block": block,
	})
	st.blocks++
	return idx
}

// canOpenThinking reports whether a thinking block may still be opened: once
// text or tool blocks have been emitted the block order is already fixed.
func (st *anthropicStreamer) canOpenThinking() bool {
	return !st.textOpen
}

// closeThinking emits the signature and stop for an open thinking block.
// Upstream reasoning arrives as OpenAI `reasoning_content`, which carries no
// signature, so we send an empty one. That is fine on this path: the proxy is
// the client's upstream, and anthropicToOpenAI drops thinking blocks when the
// history is translated back, so an unsigned block is never replayed to a real
// Anthropic API (which would reject it with a 400).
func (st *anthropicStreamer) closeThinking() {
	if st.thinkOpen {
		st.send("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": st.thinkIdx,
			"delta": map[string]any{"type": "signature_delta", "signature": ""},
		})
		st.send("content_block_stop", map[string]any{"type": "content_block_stop", "index": st.thinkIdx})
		st.thinkOpen = false
	}
}

func (st *anthropicStreamer) closeText() {
	if st.textOpen {
		st.send("content_block_stop", map[string]any{"type": "content_block_stop", "index": st.textIdx})
		st.textOpen = false
	}
}

func estimateOutputTokens(st *anthropicStreamer) int {
	if st.usage != nil {
		if v, ok := st.usage["completion_tokens"].(float64); ok {
			return int(v)
		}
	}
	return 1
}

func writeAnthropicAggregate(w http.ResponseWriter, body io.ReadCloser, model string, estIn int, emitThink bool) {
	var content strings.Builder
	var reasoning strings.Builder
	calls := map[int]*oaiToolCall{}
	order := []int{}
	finish := ""
	usage := map[string]any{}
	br := bufio.NewReader(body)
	for {
		line, err := br.ReadString('\n')
		if s := strings.TrimSpace(line); strings.HasPrefix(s, "data:") {
			data := strings.TrimSpace(strings.TrimPrefix(s, "data:"))
			if data == "[DONE]" {
				break
			}
			var ch struct {
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
				if ch.Usage != nil {
					usage = ch.Usage
				}
				for _, c := range ch.Choices {
					content.WriteString(c.Delta.Content)
					reasoning.WriteString(c.Delta.ReasoningContent)
					for _, tc := range c.Delta.ToolCalls {
						cur := calls[tc.Index]
						if cur == nil {
							cur = &oaiToolCall{}
							calls[tc.Index] = cur
							order = append(order, tc.Index)
						}
						if tc.ID != "" {
							cur.id = tc.ID
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
	sort.Ints(order)
	blocks := []any{}
	if reasoning.Len() > 0 && emitThink {
		blocks = append(blocks, map[string]any{"type": "thinking", "thinking": reasoning.String(), "signature": ""})
	}
	if content.Len() > 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": content.String()})
	}
	for _, idx := range order {
		c := calls[idx]
		if c == nil {
			continue
		}
		in := map[string]any{}
		_ = json.Unmarshal([]byte(c.args.String()), &in)
		id := c.id
		if id == "" {
			id = mintOpencodeID("toolu")
		}
		blocks = append(blocks, map[string]any{
			"type":  "tool_use",
			"id":    id,
			"name":  clientToolName(c.name),
			"input": in,
		})
	}
	if len(blocks) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": ""})
	}
	inTok, outTok := estIn, 1
	if v, ok := usage["prompt_tokens"].(float64); ok {
		inTok = int(v)
	}
	if v, ok := usage["completion_tokens"].(float64); ok {
		outTok = int(v)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            mintOpencodeID("msg"),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       blocks,
		"stop_reason":   anthropicStopReason(finish, len(order) > 0),
		"stop_sequence": nil,
		"usage":         map[string]any{"input_tokens": inTok, "output_tokens": outTok},
	})
}

// zenClientToolAlias maps injected zen tool names back to the names Claude
// Code actually exposes, so a call to the gate's proof tool still executes.
var zenClientToolAlias = map[string]string{"read": "Read"}

func clientToolName(name string) string {
	if v, ok := zenClientToolAlias[name]; ok {
		return v
	}
	return name
}

func anthropicStopReason(finish string, hasTools bool) string {
	if hasTools || finish == "tool_calls" || finish == "function_call" {
		return "tool_use"
	}
	switch finish {
	case "length":
		return "max_tokens"
	case "stop":
		return "end_turn"
	default:
		return "end_turn"
	}
}

// ---- serve lane (official opencode server) ----

func (s *Server) handleAnthropicServe(w http.ResponseWriter, r *http.Request, prov *Provider, target string, req map[string]any, clientModel string) {
	clientStream, _ := req["stream"].(bool)
	if err := ensureServe(prov); err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "serve lane down: "+err.Error())
		return
	}
	sid, err := serveCreateSession(r.Context(), prov)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "serve session: "+err.Error())
		return
	}
	defer serveKillSession(r.Context(), prov, sid)

	prompt := anthropicFlatten(req)
	t0 := time.Now()
	text, reasoning, usage, err := serveChat(r.Context(), prov, sid, target, prompt)
	if debugOn() {
		fmt.Fprintf(os.Stderr, "serve chat model=%s prompt=%d dur=%s err=%v\n", target, len(prompt), time.Since(t0), err)
	}
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	inTok, outTok := usage["prompt_tokens"], usage["completion_tokens"]
	if inTok == 0 {
		inTok = anthropicEstimateTokens([]byte(prompt))
	}
	if outTok == 0 {
		outTok = len(text) / 4
	}
	if !clientStream {
		content := []any{}
		if reasoning != "" && thinkingEnabled() && thinkingRequested(req) {
			content = append(content, map[string]any{"type": "thinking", "thinking": reasoning, "signature": ""})
		}
		content = append(content, map[string]any{"type": "text", "text": text})
		writeJSON(w, http.StatusOK, map[string]any{
			"id":            mintOpencodeID("msg"),
			"type":          "message",
			"role":          "assistant",
			"model":         clientModel,
			"content":       content,
			"stop_reason":   "end_turn",
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": inTok, "output_tokens": outTok},
		})
		return
	}

	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	send := func(event string, payload any) {
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if fl != nil {
			fl.Flush()
		}
	}
	send("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            mintOpencodeID("msg"),
			"type":          "message",
			"role":          "assistant",
			"model":         clientModel,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": inTok, "output_tokens": 1},
		},
	})
	emitServeThink := reasoning != "" && thinkingEnabled() && thinkingRequested(req)
	idx := 0
	if emitServeThink {
		send("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""},
		})
		send("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "thinking_delta", "thinking": reasoning},
		})
		send("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "signature_delta", "signature": ""},
		})
		send("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		idx = 1
	}
	send("content_block_start", map[string]any{
		"type": "content_block_start", "index": idx,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
	send("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": idx,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
	send("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
	send("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": outTok},
	})
	send("message_stop", map[string]any{"type": "message_stop"})
}

func anthropicFlatten(req map[string]any) string {
	var sb strings.Builder
	if sys := anthropicText(req["system"]); sys != "" {
		sb.WriteString("System:\n" + sys + "\n\n")
	}
	raw, _ := req["messages"].([]any)
	for _, m := range raw {
		mm, _ := m.(map[string]any)
		if mm == nil {
			continue
		}
		role := str(mm["role"])
		label := "User"
		if role == "assistant" {
			label = "Assistant"
		}
		switch c := mm["content"].(type) {
		case string:
			sb.WriteString(label + ":\n" + c + "\n\n")
		case []any:
			for _, b := range c {
				bb, _ := b.(map[string]any)
				if bb == nil {
					continue
				}
				switch str(bb["type"]) {
				case "text":
					sb.WriteString(label + ":\n" + str(bb["text"]) + "\n\n")
				case "tool_use":
					args, _ := json.Marshal(bb["input"])
					sb.WriteString("Assistant called tool " + str(bb["name"]) + " with " + string(args) + "\n\n")
				case "tool_result":
					sb.WriteString("Tool result (" + str(bb["tool_use_id"]) + "): " + anthropicToolResultText(bb["content"]) + "\n\n")
				case "image":
					sb.WriteString("[image omitted]\n\n")
				}
			}
		}
	}
	return sb.String()
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// thinkingRequested reports whether the client asked for reasoning output.
// Claude Code sends {"thinking":{"type":"adaptive","display":...}} or the
// legacy {"type":"enabled","budget_tokens":N}; anything else (including an
// absent field) keeps the response text-only.
func thinkingRequested(req map[string]any) bool {
	v, ok := req["thinking"]
	if !ok || v == nil {
		return false
	}
	t, _ := v.(map[string]any)
	switch str(t["type"]) {
	case "disabled", "":
		return false
	case "adaptive", "enabled":
		return true
	}
	return false
}

func debugOn() bool { return os.Getenv("OPENCODE_PROXY_DEBUG") != "" }

// thinkingEnabled reports whether upstream reasoning_content should be
// re-emitted as Anthropic `thinking` blocks. On by default; set
// OPENCODE_PROXY_THINKING=off to drop it and keep the old shape.
func thinkingEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("OPENCODE_PROXY_THINKING")))
	return v != "off" && v != "0" && v != "false"
}

var anthSeq atomic.Uint64
