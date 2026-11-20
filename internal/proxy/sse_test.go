package proxy

import "testing"

func feedAll(t *testing.T, acc *sseAccum, lines []string) {
	t.Helper()
	for _, l := range lines {
		acc.feed([]byte(l))
	}
}

func TestSSEChatFinalUsage(t *testing.T) {
	acc := newSSEAccum(EpChat)
	feedAll(t, acc, []string{
		`{"choices":[{"delta":{"content":"Hel"}}]}`,
		`{"choices":[{"delta":{"content":"lo"}}]}`,
		`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":4}}}`,
		`[DONE]`,
	})
	tok, ok := acc.result()
	if !ok {
		t.Fatal("expected usage")
	}
	if tok.Prompt != 11 || tok.Completion != 2 || tok.Cached != 4 {
		t.Errorf("got %+v", tok)
	}
	if acc.textChars != 5 { // "Hel" + "lo"
		t.Errorf("textChars = %d, want 5", acc.textChars)
	}
}

func TestSSEResponsesCompleted(t *testing.T) {
	acc := newSSEAccum(EpResponses)
	feedAll(t, acc, []string{
		`{"type":"response.output_text.delta","delta":"hi"}`,
		`{"type":"response.completed","response":{"usage":{"input_tokens":9,"output_tokens":3,"input_tokens_details":{"cached_tokens":2}}}}`,
	})
	tok, ok := acc.result()
	if !ok {
		t.Fatal("expected usage")
	}
	if tok.Prompt != 9 || tok.Completion != 3 || tok.Cached != 2 {
		t.Errorf("got %+v", tok)
	}
}

func TestSSEMessagesStartAndDelta(t *testing.T) {
	acc := newSSEAccum(EpMessages)
	feedAll(t, acc, []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":5,"cache_read_input_tokens":100,"cache_creation_input_tokens":1}}}`,
		`{"type":"content_block_delta","delta":{"text":"yes"}}`,
		`{"type":"message_delta","usage":{"output_tokens":8}}`,
	})
	tok, ok := acc.result()
	if !ok {
		t.Fatal("expected usage")
	}
	if tok.Prompt != 106 || tok.Cached != 100 || tok.CacheWrite != 1 || tok.Completion != 8 {
		t.Errorf("got %+v", tok)
	}
}

func TestSSENoUsage(t *testing.T) {
	acc := newSSEAccum(EpChat)
	feedAll(t, acc, []string{`{"choices":[{"delta":{"content":"x"}}]}`})
	if _, ok := acc.result(); ok {
		t.Error("expected no usage captured")
	}
	if acc.textChars != 1 {
		t.Errorf("textChars = %d, want 1", acc.textChars)
	}
}
