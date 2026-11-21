// Minimal stateless MCP (Model Context Protocol) server over streamable
// HTTP: POST /mcp with plain JSON-RPC 2.0 responses — no sessions, no SSE
// streams (both are optional in the spec; stateless is legal and fits this
// one-tool server). Exposes check_utilization so MCP-native agents can
// discover and call it without reading any docs.
package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
)

// mcpProtocolVersion is the newest protocol revision this server speaks.
// Clients negotiating an older supported version get theirs echoed back.
const mcpProtocolVersion = "2025-06-18"

var mcpSupportedVersions = map[string]bool{
	"2024-11-05": true,
	"2025-03-26": true,
	"2025-06-18": true,
}

type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func mcpError(id json.RawMessage, code int, msg string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": msg},
	}
}

func mcpResult(id json.RawMessage, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

// MCP serves POST /mcp (relay port, sk-key auth — MCP HTTP auth failures
// surface as plain HTTP 401, per spec).
func (h *Handler) MCP(w http.ResponseWriter, r *http.Request) {
	if _, _, err := h.authKey(r); err != nil {
		writeError(w, http.StatusUnauthorized, "invalid API key")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read request body")
		return
	}
	if len(body) > 1<<20 {
		// Same rule as the proxy routes: oversize is an HTTP-level
		// rejection, not a JSON parse failure.
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	var req jsonrpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusOK, mcpError(nil, -32700, "parse error"))
		return
	}
	// Notifications (absent/null id) get 202 with no body — even malformed
	// ones: a server must never answer a notification (streamable-HTTP rule),
	// so this check runs before envelope validation.
	if req.ID == nil || string(req.ID) == "null" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeJSON(w, http.StatusOK, mcpError(req.ID, -32600, "invalid request"))
		return
	}

	switch req.Method {
	case "initialize":
		version := mcpProtocolVersion
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(req.Params, &p) == nil && mcpSupportedVersions[p.ProtocolVersion] {
			version = p.ProtocolVersion
		}
		writeJSON(w, http.StatusOK, mcpResult(req.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "llm-relay", "version": "1"},
			"instructions": "Call check_utilization before delegating work to a " +
				"model on this server: status free means there is capacity, " +
				"busy means wait or pick another model, offline means skip.",
		}))
	case "ping":
		writeJSON(w, http.StatusOK, mcpResult(req.ID, map[string]any{}))
	case "tools/list":
		writeJSON(w, http.StatusOK, mcpResult(req.ID, map[string]any{
			"tools": []any{map[string]any{
				"name": "check_utilization",
				"description": "Check whether models on this LLM relay are free " +
					"before delegating a task. Returns per model: online, " +
					"active streams, concurrency_limit, utilization_pct (mean " +
					"busy % over the last 5 minutes) and a derived status: " +
					"free, busy or offline.",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"model": map[string]any{
							"type":        "string",
							"description": "optional: only report this model name",
						},
					},
					"additionalProperties": false,
				},
			}},
		}))
	case "tools/call":
		h.mcpToolCall(w, r.Context(), req)
	default:
		writeJSON(w, http.StatusOK, mcpError(req.ID, -32601, "method not found: "+req.Method))
	}
}

func (h *Handler) mcpToolCall(w http.ResponseWriter, ctx context.Context, req jsonrpcRequest) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeJSON(w, http.StatusOK, mcpError(req.ID, -32602, "invalid params"))
		return
	}
	if params.Name != "check_utilization" {
		// Unknown tools are a protocol error (spec: -32602 with an
		// "Unknown tool" message); in-band isError is for execution failures.
		writeJSON(w, http.StatusOK, mcpError(req.ID, -32602, "Unknown tool: "+params.Name))
		return
	}
	filter := ""
	if v, ok := params.Arguments["model"]; ok {
		s, ok := v.(string)
		if !ok {
			writeJSON(w, http.StatusOK, mcpError(req.ID, -32602,
				"model filter must be a string"))
			return
		}
		filter = s
	}

	payload, err := h.Utilization(ctx)
	if err != nil {
		// In-band so the agent sees the failure, but the raw error stays
		// server-side: it can carry SQLite/driver text (the REST sibling
		// masks the same failure with "internal error").
		log.Printf("relay: mcp check_utilization failed: %v", err)
		writeJSON(w, http.StatusOK, mcpResult(req.ID, map[string]any{
			"isError": true,
			"content": []any{map[string]any{"type": "text",
				"text": "utilization check failed (internal error)"}},
		}))
		return
	}
	if filter != "" {
		models := []ModelStatus{}
		all, _ := payload["models"].([]ModelStatus)
		for _, m := range all {
			if m.Name == filter {
				models = append(models, m)
			}
		}
		if len(models) == 0 {
			// Ambiguity guard: an empty list would be indistinguishable
			// from "no enabled models" for a typo'd name.
			writeJSON(w, http.StatusOK, mcpResult(req.ID, map[string]any{
				"isError": true,
				"content": []any{map[string]any{"type": "text",
					"text": "no enabled model named: " + filter}},
			}))
			return
		}
		payload = map[string]any{
			"window_minutes": payload["window_minutes"],
			"models":         models,
			"filter":         filter,
		}
	}
	text, _ := json.MarshalIndent(payload, "", "  ")
	writeJSON(w, http.StatusOK, mcpResult(req.ID, map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(text)}},
		"structuredContent": payload,
		"isError":           false,
	}))
}
