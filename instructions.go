package main

import (
	"net/http"
	"strings"
)

const instructionsHeader = "X-Custom-Instructions"

func (s *Server) instructionsFor(r *http.Request) string {
	if h := strings.TrimSpace(r.Header.Get(instructionsHeader)); h != "" {
		return h
	}
	return strings.TrimSpace(s.cfg.Server.Instructions)
}

func applyInstructions(req map[string]any, instructions string) {
	instructions = strings.TrimSpace(instructions)
	if instructions == "" {
		return
	}
	msgs, _ := req["messages"].([]any)
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm == nil || mm["role"] != "system" {
			continue
		}
		switch c := mm["content"].(type) {
		case string:
			if strings.TrimSpace(c) == "" {
				mm["content"] = instructions
			} else {
				mm["content"] = c + "\n\n" + instructions
			}
		case []any:
			mm["content"] = append(c, map[string]any{"type": "text", "text": "\n\n" + instructions})
		default:
			mm["content"] = instructions
		}
		return
	}
	sys := map[string]any{"role": "system", "content": instructions}
	req["messages"] = append([]any{sys}, msgs...)
}
