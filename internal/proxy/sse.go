// SSE scanning: while the stream is copied to the client, each `data:` payload
// is fed to the dialect accumulator to capture usage and visible-text stats.
package proxy

import (
	"encoding/json"
	"strings"

	"llm-relay/internal/pricing"
)

type sseAccum struct {
	endpoint string

	oai  pricing.Tokens // chat / completions / responses: last usage seen
	oaiF bool

	anthIn  pricing.Tokens // messages: from message_start
	anthOut int64          // messages: from message_delta
	anthF   bool

	textChars int64
	textWords int64
}

func newSSEAccum(endpoint string) *sseAccum { return &sseAccum{endpoint: endpoint} }

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
					Content string `json:"content"`
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
		case "response.completed":
			if v.Response != nil {
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
					a.anthIn, a.anthF = tok, true
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
					a.anthF = true
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
	a.textChars += int64(len(s))
	a.textWords += int64(len(strings.Fields(s)))
}

// result returns the captured usage (found=false → caller should estimate).
func (a *sseAccum) result() (pricing.Tokens, bool) {
	switch a.endpoint {
	case EpMessages:
		if !a.anthF {
			return pricing.Tokens{}, false
		}
		tok := a.anthIn
		tok.Completion = a.anthOut
		return tok, true
	default:
		return a.oai, a.oaiF
	}
}
