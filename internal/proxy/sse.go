// SSE scanning: while the stream is copied to the client, each `data:` payload
// is fed to the dialect accumulator to capture usage and visible-text stats.
package proxy

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"llm-relay/internal/pricing"
)

type sseAccum struct {
	endpoint string

	oai  pricing.Tokens // chat / completions / responses: last usage seen
	oaiF bool

	anthIn   pricing.Tokens // messages: from message_start
	anthInF  bool
	anthOut  int64 // messages: from message_delta
	anthOutF bool

	textChars int64
	textWords int64
}

func newSSEAccum(endpoint string) *sseAccum { return &sseAccum{endpoint: endpoint} }

// StreamUsage runs the dialect accumulator over a captured SSE body and
// returns the usage it saw (the federation hub meters transit traffic
// from the response frames it pumps, so it parses after the fact).
// ok=false when the stream carried no usage payload.
func StreamUsage(endpoint string, raw []byte) (tok pricing.Tokens, ok bool) {
	acc := newSSEAccum(endpoint)
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data:") {
			acc.feed([]byte(strings.TrimSpace(trimmed[5:])))
		}
	}
	return acc.result()
}

// feed parses one SSE `data:` payload (without the "data:" prefix).
func (a *sseAccum) feed(payload []byte) {
	if len(payload) == 0 || string(payload) == "[DONE]" {
		return
	}
	switch a.endpoint {
	case EpChat:
		var v struct {
			Usage   *json.RawMessage `json:"usage"`
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					Reasoning string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal(payload, &v) != nil {
			return
		}
		if v.Usage != nil {
			if tok, ok := ExtractUsage(EpChat, payload); ok {
				a.oai, a.oaiF = tok, true
			}
		}
		for _, c := range v.Choices {
			a.addText(c.Delta.Content)
			// reasoning models stream thinking here; the non-streaming
			// equivalent counts it too (completionTextStats).
			a.addText(c.Delta.Reasoning)
		}

	case EpCompletions:
		var v struct {
			Usage   *json.RawMessage `json:"usage"`
			Choices []struct {
				Text string `json:"text"`
			} `json:"choices"`
		}
		if json.Unmarshal(payload, &v) != nil {
			return
		}
		if v.Usage != nil {
			if tok, ok := ExtractUsage(EpCompletions, payload); ok {
				a.oai, a.oaiF = tok, true
			}
		}
		for _, c := range v.Choices {
			a.addText(c.Text)
		}

	case EpResponses:
		var v struct {
			Type     string `json:"type"`
			Delta    string `json:"delta"`
			Response *struct {
				Usage json.RawMessage `json:"usage"`
			} `json:"response"`
		}
		if json.Unmarshal(payload, &v) != nil {
			return
		}
		switch v.Type {
		case "response.output_text.delta":
			a.addText(v.Delta)
		case "response.completed", "response.incomplete":
			// incomplete (e.g. max_tokens hit) still carries real usage.
			if v.Response != nil && len(v.Response.Usage) > 0 {
				wrapped := []byte(`{"usage":` + string(v.Response.Usage) + `}`)
				if tok, ok := ExtractUsage(EpResponses, wrapped); ok {
					a.oai, a.oaiF = tok, true
				}
			}
		}

	case EpMessages:
		var v struct {
			Type    string `json:"type"`
			Message *struct {
				Usage json.RawMessage `json:"usage"`
			} `json:"message"`
			Usage json.RawMessage `json:"usage"`
			Delta *struct {
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal(payload, &v) != nil {
			return
		}
		switch v.Type {
		case "message_start":
			if v.Message != nil && len(v.Message.Usage) > 0 {
				wrapped := []byte(`{"usage":` + string(v.Message.Usage) + `}`)
				if tok, ok := ExtractUsage(EpMessages, wrapped); ok {
					a.anthIn, a.anthInF = tok, true
				}
			}
		case "message_delta":
			if len(v.Usage) > 0 {
				wrapped := []byte(`{"usage":` + string(v.Usage) + `}`)
				var probe struct {
					Usage *struct {
						Output int64 `json:"output_tokens"`
					} `json:"usage"`
				}
				if json.Unmarshal(wrapped, &probe) == nil && probe.Usage != nil {
					a.anthOut = probe.Usage.Output
					a.anthOutF = true
				}
			}
		case "content_block_delta":
			if v.Delta != nil {
				a.addText(v.Delta.Text)
			}
		}
	}
}

func (a *sseAccum) addText(s string) {
	if s == "" {
		return
	}
	// rune count, not bytes: the estimator's divisor is chars-per-token and
	// multi-byte output must not inflate it. (Words split across chunk
	// boundaries count twice — a small upward bias on the word floor.)
	a.textChars += int64(utf8.RuneCountInString(s))
	a.textWords += int64(len(strings.Fields(s)))
}

// result returns the captured usage (found=false → caller should estimate).
func (a *sseAccum) result() (pricing.Tokens, bool) {
	switch a.endpoint {
	case EpMessages:
		switch {
		case a.anthInF && a.anthOutF:
			tok := a.anthIn
			tok.Completion = a.anthOut
			return tok, true
		case a.anthOutF && a.anthOut > 0:
			// Delta only: the capture window missed message_start (a
			// tail-capped transit body keeps the stream's end, where
			// message_delta lives). The final output count is real; the
			// input side is unknown and stays 0 — a partial ledger row
			// beats the caller's alternative of no usage at all.
			return pricing.Tokens{Completion: a.anthOut}, true
		default:
			// message_start only (or nothing): its output_tokens is the
			// initial placeholder (~1), not the final count — claiming it
			// would bill the completion at ~1 instead of letting the caller
			// estimate.
			return pricing.Tokens{}, false
		}
	default:
		return a.oai, a.oaiF
	}
}
