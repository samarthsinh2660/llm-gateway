package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openziti/llm-gateway/providers"
)

func doChat(t *testing.T, h http.Handler, req providers.ChatCompletionRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	httpReq := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httpReq)
	return rr
}

func userReq(model, content string) providers.ChatCompletionRequest {
	return providers.ChatCompletionRequest{
		Model:    model,
		Messages: []providers.Message{{Role: "user", Content: content}},
	}
}

func TestNonStreamingEcho(t *testing.T) {
	h := newServer(config{}).handler()
	rr := doChat(t, h, userReq("dummy", "hello world"))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp providers.ChatCompletionResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", resp.Object)
	}
	if resp.Model != "dummy" {
		t.Errorf("model = %q, want dummy", resp.Model)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(resp.Choices))
	}
	choice := resp.Choices[0]
	if choice.FinishReason == nil || *choice.FinishReason != "stop" {
		t.Errorf("finish_reason = %v, want stop", choice.FinishReason)
	}
	if choice.Message == nil || !strings.Contains(choice.Message.GetContentString(), "hello world") {
		t.Errorf("message did not echo the prompt: %+v", choice.Message)
	}
	if resp.Usage == nil {
		t.Fatal("usage missing")
	}
	if resp.Usage.PromptTokens != 2 {
		t.Errorf("prompt_tokens = %d, want 2", resp.Usage.PromptTokens)
	}
	if resp.Usage.TotalTokens != resp.Usage.PromptTokens+resp.Usage.CompletionTokens {
		t.Errorf("total_tokens %d != prompt %d + completion %d", resp.Usage.TotalTokens, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	}
}

func TestCannedResponse(t *testing.T) {
	h := newServer(config{response: "canned reply"}).handler()
	rr := doChat(t, h, userReq("dummy", "ignored"))

	var resp providers.ChatCompletionResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := resp.Choices[0].Message.GetContentString(); got != "canned reply" {
		t.Errorf("content = %q, want %q", got, "canned reply")
	}
}

func TestModelsEndpoint(t *testing.T) {
	h := newServer(config{models: []string{"dummy-a", "dummy-b"}}).handler()
	req := httptest.NewRequest("GET", "/v1/models", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp providers.ModelsResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Object != "list" {
		t.Errorf("object = %q, want list", resp.Object)
	}
	if len(resp.Data) != 2 || resp.Data[0].ID != "dummy-a" || resp.Data[1].ID != "dummy-b" {
		t.Errorf("data = %+v, want dummy-a, dummy-b", resp.Data)
	}
	if resp.Data[0].OwnedBy != "dummy" {
		t.Errorf("owned_by = %q, want dummy", resp.Data[0].OwnedBy)
	}
}

// parseSSE collects content, the final finish_reason, the tool-call name (if
// any), and whether [DONE] was seen from an SSE response body.
func parseSSE(t *testing.T, body string) (content, finish, toolName string, sawDone bool) {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			sawDone = true
			continue
		}
		var chunk providers.StreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("unmarshal chunk %q: %v", data, err)
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		ch := chunk.Choices[0]
		if ch.Delta != nil {
			content += ch.Delta.Content
			if len(ch.Delta.ToolCalls) > 0 && ch.Delta.ToolCalls[0].Function.Name != "" {
				toolName = ch.Delta.ToolCalls[0].Function.Name
			}
		}
		if ch.FinishReason != nil {
			finish = *ch.FinishReason
		}
	}
	return
}

func TestStreamingEcho(t *testing.T) {
	h := newServer(config{}).handler()
	req := userReq("dummy", "hello world")
	req.Stream = true
	rr := doChat(t, h, req)

	if ct := rr.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	content, finish, _, sawDone := parseSSE(t, rr.Body.String())
	if !strings.Contains(content, "hello world") {
		t.Errorf("streamed content = %q, want it to contain the prompt", content)
	}
	if finish != "stop" {
		t.Errorf("finish_reason = %q, want stop", finish)
	}
	if !sawDone {
		t.Error("missing [DONE] sentinel")
	}
}

func toolReq(stream bool) providers.ChatCompletionRequest {
	req := userReq("dummy", "what's the weather?")
	req.Stream = stream
	req.Tools = []providers.Tool{{
		Type:     "function",
		Function: providers.Function{Name: "get_weather", Description: "get the weather"},
	}}
	return req
}

func TestToolCallNonStreaming(t *testing.T) {
	h := newServer(config{}).handler()
	rr := doChat(t, h, toolReq(false))

	var resp providers.ChatCompletionResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	choice := resp.Choices[0]
	if choice.FinishReason == nil || *choice.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls", choice.FinishReason)
	}
	if choice.Message == nil || len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("expected one tool call, got %+v", choice.Message)
	}
	if got := choice.Message.ToolCalls[0].Function.Name; got != "get_weather" {
		t.Errorf("tool name = %q, want get_weather", got)
	}
}

func TestToolCallStreaming(t *testing.T) {
	h := newServer(config{}).handler()
	rr := doChat(t, h, toolReq(true))

	_, finish, toolName, sawDone := parseSSE(t, rr.Body.String())
	if finish != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", finish)
	}
	if toolName != "get_weather" {
		t.Errorf("tool name = %q, want get_weather", toolName)
	}
	if !sawDone {
		t.Error("missing [DONE] sentinel")
	}
}

func TestErrorInjection(t *testing.T) {
	tests := []struct {
		name       string
		errorType  string
		wantStatus int
	}{
		{"rate limit", "rate_limit_error", http.StatusTooManyRequests},
		{"server error", "server_error", http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newServer(config{errorRate: 1, errorType: tt.errorType}).handler()
			rr := doChat(t, h, userReq("dummy", "hi"))
			if rr.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rr.Code, tt.wantStatus)
			}
			var errResp providers.ErrorResponse
			if err := json.Unmarshal(rr.Body.Bytes(), &errResp); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			if errResp.Error.Type != tt.errorType {
				t.Errorf("error type = %q, want %q", errResp.Error.Type, tt.errorType)
			}
		})
	}
}

func TestBadRequests(t *testing.T) {
	h := newServer(config{}).handler()

	// invalid JSON
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("{not json"))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON status = %d, want 400", rr.Code)
	}

	// empty messages
	rr = doChat(t, h, providers.ChatCompletionRequest{Model: "dummy"})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("empty messages status = %d, want 400", rr.Code)
	}
}
