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
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not read request body")
		return
	}
	var req jsonrpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusOK, mcpError(nil, -32700, "parse error"))
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeJSON(w, http.StatusOK, mcpError(req.ID, -32600, "invalid request"))
		return
	}
	// Notifications get 202 with no body (streamable-HTTP rule).
	if req.ID == nil || string(req.ID) == "null" {
		w.WriteHeader(http.StatusAccepted)
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
	filter, _ := params.Arguments["model"].(string)

	payload, err := h.Utilization(ctx)
	if err != nil {
		// Tool execution failures come back in-band so the agent can read why.
		writeJSON(w, http.StatusOK, mcpResult(req.ID, map[string]any{
			"isError": true,
			"content": []any{map[string]any{"type": "text",
				"text": "utilization check failed: " + err.Error()}},
		}))
		return
	}
	if filter != "" {
		models := []ModelStatus{}
		for _, m := range payload["models"].([]ModelStatus) {
			if m.Name == filter {
				models = append(models, m)
			}
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
