package openai

import "testing"

func TestNativeCompletionResponsePreserved(t *testing.T) {
	body := []byte(`{"object":"text_completion","choices":[{"index":0,"text":"native","logprobs":{"tokens":["native"]},"finish_reason":null}],"extension":true}`)
	if got := convertChatCompletionsResponseToCompletions(body); string(got) != string(body) {
		t.Fatalf("native JSON changed: %s", got)
	}
	if got := convertChatCompletionsStreamChunkToCompletions(body); string(got) != string(body) {
		t.Fatalf("native SSE payload changed: %s", got)
	}
}
