// Dialect definitions: per-endpoint usage extraction and session derivation.
package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"llm-relay/internal/pricing"
)

// Endpoint identifiers stored in usage_log.endpoint.
const (
	EpChat        = "chat"
	EpCompletions = "completions"
	EpResponses   = "responses"
	EpMessages    = "messages"
	EpEmbeddings  = "embeddings"
)

// ExtractUsage parses the usage block of a non-streaming response body for
// the given endpoint. ok=false when the response carries no usage at all.
func ExtractUsage(endpoint string, body []byte) (tok pricing.Tokens, ok bool) {
	switch endpoint {
	case EpChat, EpCompletions, EpEmbeddings:
		var v struct {
			Usage *struct {
				PromptTokens     int64 `json:"prompt_tokens"`
				CompletionTokens int64 `json:"completion_tokens"`
				Details          *struct {
					CachedTokens int64 `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(body, &v); err != nil || v.Usage == nil {
			return tok, false
		}
		tok.Prompt = v.Usage.PromptTokens
		tok.Completion = v.Usage.CompletionTokens
		if v.Usage.Details != nil {
			tok.Cached = v.Usage.Details.CachedTokens
		}
		return tok, true

	case EpResponses:
		var v struct {
			Usage *struct {
				Input   int64 `json:"input_tokens"`
				Output  int64 `json:"output_tokens"`
				Details *struct {
					Cached int64 `json:"cached_tokens"`
				} `json:"input_tokens_details"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(body, &v); err != nil || v.Usage == nil {
			return tok, false
		}
		tok.Prompt = v.Usage.Input
		tok.Completion = v.Usage.Output
		if v.Usage.Details != nil {
			tok.Cached = v.Usage.Details.Cached
		}
		return tok, true

	case EpMessages:
		var v struct {
			Usage *struct {
				Input         int64 `json:"input_tokens"`
				Output        int64 `json:"output_tokens"`
				CacheRead     int64 `json:"cache_read_input_tokens"`
				CacheCreation int64 `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(body, &v); err != nil || v.Usage == nil {
			return tok, false
		}
		u := v.Usage
		// Anthropic input_tokens EXCLUDES the cache buckets; normalize to the
		// OpenAI convention where Prompt is the total input.
		tok.Prompt = u.Input + u.CacheRead + u.CacheCreation
		tok.Completion = u.Output
		tok.Cached = u.CacheRead
		tok.CacheWrite = u.CacheCreation
		return tok, true
	}
	return tok, false
}

// DeriveSession returns a stable session id for a conversation-shaped request
// when the client did not send X-Session-Id: hash(key, model, first user
// message) — every follow-up turn of the same conversation carries the same
// first message, so they group together. Returns "" when no user message can
// be found (or the endpoint is not conversation-shaped).
func DeriveSession(endpoint string, body []byte, keyID int64, model string) string {
	var text string
	switch endpoint {
	case EpChat, EpCompletions:
		var v struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
			Prompt string `json:"prompt"`
		}
		if json.Unmarshal(body, &v) == nil {
			for _, m := range v.Messages {
				if m.Role == "user" {
					text = flattenContent(m.Content)
					break
				}
			}
			if text == "" && v.Prompt != "" {
				text = v.Prompt
			}
		}
	case EpResponses:
		var v struct {
			Input json.RawMessage `json:"input"`
		}
		if json.Unmarshal(body, &v) == nil {
			text = firstUserInResponsesInput(v.Input)
		}
	case EpMessages:
		var v struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if json.Unmarshal(body, &v) == nil {
			for _, m := range v.Messages {
				if m.Role == "user" {
					text = flattenContent(m.Content)
					break
				}
			}
		}
	}
	if text == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s\x00%s", keyID, model, text)))
	return hex.EncodeToString(sum[:])[:16]
}

// flattenContent renders message content (string or content-part array) to plain text.
func flattenContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		out := ""
		for _, p := range parts {
			if p.Type == "text" || p.Text != "" {
				out += p.Text
			}
		}
		return out
	}
	return ""
}

func firstUserInResponsesInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var items []struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &items) != nil {
		return ""
	}
	for _, it := range items {
		if it.Role == "user" && (it.Type == "" || it.Type == "message") {
			// Skip empty user turns (e.g. image-only) so the session hash
			// keys off the first turn that actually carries text.
			if text := flattenContent(it.Content); text != "" {
				return text
			}
		}
	}
	return ""
}

// completionTextStats extracts visible completion text stats from a
// non-streaming response body, for the estimation fallback.
func completionTextStats(endpoint string, body []byte) (chars, words int64) {
	add := func(s string) {
		chars += int64(utf8.RuneCountInString(s))
		words += int64(len(strings.Fields(s)))
	}
	switch endpoint {
	case EpChat:
		var v struct {
			Choices []struct {
				Message struct {
					Content   string `json:"content"`
					Reasoning string `json:"reasoning_content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if json.Unmarshal(body, &v) == nil {
			for _, c := range v.Choices {
				add(c.Message.Content)
				add(c.Message.Reasoning)
			}
		}
	case EpCompletions:
		var v struct {
			Choices []struct {
				Text string `json:"text"`
			} `json:"choices"`
		}
		if json.Unmarshal(body, &v) == nil {
			for _, c := range v.Choices {
				add(c.Text)
			}
		}
	case EpResponses:
		var v struct {
			OutputText []string `json:"output_text"`
			Output     []struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
		}
		if json.Unmarshal(body, &v) == nil {
			for _, s := range v.OutputText {
				add(s)
			}
			if len(v.OutputText) == 0 {
				for _, o := range v.Output {
					for _, c := range o.Content {
						add(c.Text)
					}
				}
			}
		}
	case EpMessages:
		var v struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(body, &v) == nil {
			for _, c := range v.Content {
				add(c.Text)
			}
		}
	}
	return chars, words
}

// EstimateTokens is the fallback when upstream reports no usage: ~4 chars per
// token, plus a word-count floor for short completions.
func EstimateTokens(promptChars, completionTextChars, completionWords int64) pricing.Tokens {
	t := pricing.Tokens{Prompt: promptChars / 4}
	est := completionTextChars / 4
	if completionWords > est {
		est = completionWords
	}
	t.Completion = est
	return t
}
