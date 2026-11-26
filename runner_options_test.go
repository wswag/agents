package agents

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRequestOptionsSentToProviders(t *testing.T) {
	for _, mode := range []ConversationMode{ConversationModeChatCompletions, ConversationModeResponses} {
		t.Run(string(mode), func(t *testing.T) {
			var request map[string]any
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				body := `{"id":"chat","choices":[{"message":{"content":"ok"}}]}`
				if mode == ConversationModeResponses {
					body = `{"id":"resp","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}
			runner := NewRunner(RunnerConfig{APIKey: "test", BaseURL: "http://example.com", HTTPClient: client})
			agent := Agent{Key: "test", Model: "model", SystemPrompt: "system", InputTemplate: "input", Options: map[string]any{"custom_provider_field": map[string]any{"enabled": true}, "model": "bad", "temperature": 0.2}}
			_, err := runner.Run(context.Background(), agent, nil, RunOptions{ConversationMode: mode, RequestOptions: map[string]any{"model": "bad2", "temperature": 0.8, "messages": "bad", "input": "bad"}})
			if err != nil {
				t.Fatal(err)
			}
			if request["model"] != "model" || request["temperature"] != 0.8 || request["custom_provider_field"] == nil {
				t.Fatalf("unexpected request: %#v", request)
			}
			if _, ok := request["metadata"]; ok {
				t.Fatalf("unexpected metadata: %#v", request)
			}
			if _, ok := request["prompt_cache_key"]; ok {
				t.Fatalf("unexpected cache key: %#v", request)
			}
			if mode == ConversationModeChatCompletions && request["messages"] == "bad" {
				t.Fatal("messages overwritten")
			}
			if mode == ConversationModeResponses && request["input"] == "bad" {
				t.Fatal("input overwritten")
			}
		})
	}
}
