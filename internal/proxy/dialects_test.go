package proxy

import (
	"testing"
)

func TestExtractUsageChat(t *testing.T) {
	body := []byte(`{"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":34,
		"prompt_tokens_details":{"cached_tokens":80}}}`)
	tok, ok := ExtractUsage(EpChat, body)
	if !ok {
		t.Fatal("expected usage")
	}
	if tok.Prompt != 120 || tok.Completion != 34 || tok.Cached != 80 {
		t.Errorf("got %+v", tok)
	}
}

func TestExtractUsageResponses(t *testing.T) {
	body := []byte(`{"id":"resp_1","usage":{"input_tokens":200,"output_tokens":50,
		"input_tokens_details":{"cached_tokens":150}}}`)
	tok, ok := ExtractUsage(EpResponses, body)
	if !ok {
		t.Fatal("expected usage")
	}
	if tok.Prompt != 200 || tok.Completion != 50 || tok.Cached != 150 {
		t.Errorf("got %+v", tok)
	}
}

func TestExtractUsageMessages(t *testing.T) {
	body := []byte(`{"usage":{"input_tokens":10,"output_tokens":7,
		"cache_read_input_tokens":100,"cache_creation_input_tokens":5}}`)
	tok, ok := ExtractUsage(EpMessages, body)
	if !ok {
		t.Fatal("expected usage")
	}
	// Anthropic input excludes cache buckets; normalized prompt = total.
	if tok.Prompt != 115 || tok.Completion != 7 || tok.Cached != 100 || tok.CacheWrite != 5 {
		t.Errorf("got %+v", tok)
	}
}

func TestExtractUsageMissing(t *testing.T) {
	if _, ok := ExtractUsage(EpChat, []byte(`{"choices":[]}`)); ok {
		t.Error("expected no usage")
	}
}

func TestDeriveSessionStable(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"s"},{"role":"user","content":"hello"}]}`)
	s1 := DeriveSession(EpChat, body, 1, "m")
	s2 := DeriveSession(EpChat, append([]byte{}, body...), 1, "m")
	if s1 == "" || s1 != s2 {
		t.Fatalf("session unstable: %q vs %q", s1, s2)
	}
	// Same conversation, follow-up turn appended → same session.
	body2 := []byte(`{"model":"m","messages":[{"role":"system","content":"s"},{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"more"}]}`)
	if s3 := DeriveSession(EpChat, body2, 1, "m"); s3 != s1 {
		t.Errorf("follow-up turn changed session: %q vs %q", s3, s1)
	}
	// Different first message → different session.
	body3 := []byte(`{"model":"m","messages":[{"role":"user","content":"other"}]}`)
	if s4 := DeriveSession(EpChat, body3, 1, "m"); s4 == s1 {
		t.Error("different conversations share a session")
	}
	// Different key → different session even with same text.
	if s5 := DeriveSession(EpChat, body, 2, "m"); s5 == s1 {
		t.Error("different keys share a session")
	}
}

func TestDeriveSessionAnthropic(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi there"}]}]}`)
	if s := DeriveSession(EpMessages, body, 1, "m"); s == "" {
		t.Error("expected session for anthropic content blocks")
	}
}

func TestDeriveSessionNone(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"s"}]}`)
	if s := DeriveSession(EpChat, body, 1, "m"); s != "" {
		t.Errorf("expected empty session, got %q", s)
	}
}
