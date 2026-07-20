package main

import (
	"encoding/json"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/openziti/llm-gateway/providers"
)

// config holds the dummy server's runtime configuration.
type config struct {
	models      []string      // model ids advertised by /v1/models; the first is the default
	response    string        // canned reply text; empty means echo the last user message
	streamDelay time.Duration // delay inserted between streamed chunks
	errorRate   float64       // probability [0,1] that a chat request fails
	errorType   string        // OpenAI error type to inject (e.g. "rate_limit_error")
}

// server is a fake OpenAI-compatible backend. It performs no real inference and
// returns deterministic responses, so it can stand in for a model in tests and
// demos. It reuses the wire types and SSE/error helpers from the providers
// package so its output matches what the gateway expects from a real backend.
type server struct {
	cfg  config
	rand *rand.Rand
}

func newServer(cfg config) *server {
	if len(cfg.models) == 0 {
		cfg.models = []string{"dummy"}
	}
	if cfg.errorType == "" {
		cfg.errorType = providers.ErrorTypeRateLimit
	}
	return &server{
		cfg:  cfg,
		rand: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// handler wires the OpenAI-compatible routes. Shared by main and tests.
func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("GET /health", s.handleHealth)

	// keep the error surface OpenAI-shaped like the gateway's: no plain-text
	// 404/405 from the mux.
	mux.HandleFunc("/", providers.HandleNotFound)
	for _, path := range []string{"/v1/models", "/v1/chat/completions", "/health"} {
		mux.HandleFunc(path, providers.HandleMethodNotAllowed)
	}
	return mux
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

func (s *server) handleModels(w http.ResponseWriter, _ *http.Request) {
	created := time.Now().Unix()
	data := make([]providers.Model, 0, len(s.cfg.models))
	for _, id := range s.cfg.models {
		data = append(data, providers.Model{
			ID:      id,
			Object:  "model",
			Created: created,
			OwnedBy: "dummy",
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(providers.ModelsResponse{Object: "list", Data: data})
}

func (s *server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var req providers.ChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		providers.WriteError(w, providers.ErrInvalidJSON, http.StatusBadRequest)
		return
	}
	if len(req.Messages) == 0 {
		providers.WriteError(w, providers.ErrMessagesRequired, http.StatusBadRequest)
		return
	}

	// error injection: fail a fraction of requests before producing output so
	// clients' error-handling and retry logic can be exercised.
	if s.cfg.errorRate > 0 && s.rand.Float64() < s.cfg.errorRate {
		apiErr := providers.NewAPIError("dummy injected error", s.cfg.errorType)
		providers.WriteError(w, apiErr, providers.StatusCodeForError(s.cfg.errorType))
		return
	}

	model := req.Model
	if model == "" {
		model = s.cfg.models[0]
	}

	if wantsToolCall(&req) {
		s.respondToolCall(w, &req, model)
		return
	}
	s.respondText(w, &req, model)
}

// respondText answers with a plain assistant message, streaming or not.
func (s *server) respondText(w http.ResponseWriter, req *providers.ChatCompletionRequest, model string) {
	reply := s.replyText(req)
	promptTokens := promptWords(req.Messages)
	completionTokens := wordCount(reply)
	id := s.completionID("chatcmpl")
	created := time.Now().Unix()

	if !req.Stream {
		resp := &providers.ChatCompletionResponse{
			ID:      id,
			Object:  "chat.completion",
			Created: created,
			Model:   model,
			Choices: []providers.Choice{{
				Index:        0,
				Message:      &providers.Message{Role: "assistant", Content: reply},
				FinishReason: strPtr("stop"),
			}},
			Usage: &providers.Usage{
				PromptTokens:     promptTokens,
				CompletionTokens: completionTokens,
				TotalTokens:      promptTokens + completionTokens,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}

	sse := providers.NewSSEWriter(w)
	if sse == nil {
		providers.WriteError(w, providers.NewAPIError("streaming not supported", providers.ErrorTypeServer), http.StatusInternalServerError)
		return
	}
	sse.WriteHeaders()

	s.writeChunk(sse, id, created, model, providers.Delta{Role: "assistant"}, "")
	for _, piece := range splitStream(reply) {
		if s.cfg.streamDelay > 0 {
			time.Sleep(s.cfg.streamDelay)
		}
		s.writeChunk(sse, id, created, model, providers.Delta{Content: piece}, "")
	}
	s.writeChunk(sse, id, created, model, providers.Delta{}, "stop")
	sse.WriteDone()
}

// respondToolCall simulates a tool call by emitting the first offered tool's
// function name with deterministic empty arguments. It exercises the gateway's
// tool-calling translation path without any real model.
func (s *server) respondToolCall(w http.ResponseWriter, req *providers.ChatCompletionRequest, model string) {
	fnName := req.Tools[0].Function.Name
	const args = "{}"
	callID := s.completionID("call")
	id := s.completionID("chatcmpl")
	created := time.Now().Unix()
	promptTokens := promptWords(req.Messages)

	if !req.Stream {
		resp := &providers.ChatCompletionResponse{
			ID:      id,
			Object:  "chat.completion",
			Created: created,
			Model:   model,
			Choices: []providers.Choice{{
				Index: 0,
				Message: &providers.Message{
					Role: "assistant",
					ToolCalls: []providers.ToolCall{{
						ID:       callID,
						Type:     "function",
						Function: providers.FunctionCall{Name: fnName, Arguments: args},
					}},
				},
				FinishReason: strPtr("tool_calls"),
			}},
			Usage: &providers.Usage{
				PromptTokens:     promptTokens,
				CompletionTokens: 1,
				TotalTokens:      promptTokens + 1,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}

	sse := providers.NewSSEWriter(w)
	if sse == nil {
		providers.WriteError(w, providers.NewAPIError("streaming not supported", providers.ErrorTypeServer), http.StatusInternalServerError)
		return
	}
	sse.WriteHeaders()

	idx := 0
	s.writeChunk(sse, id, created, model, providers.Delta{Role: "assistant"}, "")
	s.writeChunk(sse, id, created, model, providers.Delta{
		ToolCalls: []providers.ToolCall{{
			Index:    &idx,
			ID:       callID,
			Type:     "function",
			Function: providers.FunctionCall{Name: fnName},
		}},
	}, "")
	if s.cfg.streamDelay > 0 {
		time.Sleep(s.cfg.streamDelay)
	}
	s.writeChunk(sse, id, created, model, providers.Delta{
		ToolCalls: []providers.ToolCall{{
			Index:    &idx,
			Function: providers.FunctionCall{Arguments: args},
		}},
	}, "")
	s.writeChunk(sse, id, created, model, providers.Delta{}, "tool_calls")
	sse.WriteDone()
}

func (s *server) writeChunk(sse *providers.SSEWriter, id string, created int64, model string, delta providers.Delta, finish string) {
	// empty finish reason means an intermediate chunk: emit null, not a value.
	var finishReason *string
	if finish != "" {
		finishReason = &finish
	}
	sse.WriteChunk(&providers.StreamChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []providers.Choice{{
			Index:        0,
			Delta:        &delta,
			FinishReason: finishReason,
		}},
	})
}

func strPtr(s string) *string { return &s }

// replyText returns the canned response when configured, otherwise echoes the
// last user message.
func (s *server) replyText(req *providers.ChatCompletionRequest) string {
	if s.cfg.response != "" {
		return s.cfg.response
	}
	if last := lastUserText(req.Messages); last != "" {
		return "you said: " + last
	}
	return "hello from the dummy model"
}

func (s *server) completionID(prefix string) string {
	return prefix + "-dummy-" + strconv.FormatInt(s.rand.Int63(), 36)
}

// wantsToolCall reports whether the request offers tools and hasn't disabled
// tool use via tool_choice: "none".
func wantsToolCall(req *providers.ChatCompletionRequest) bool {
	if len(req.Tools) == 0 {
		return false
	}
	if tc, ok := req.ToolChoice.(string); ok && tc == "none" {
		return false
	}
	return true
}

// lastUserText returns the text of the last user message, falling back to the
// last message of any role.
func lastUserText(msgs []providers.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return messageText(&msgs[i])
		}
	}
	if len(msgs) > 0 {
		return messageText(&msgs[len(msgs)-1])
	}
	return ""
}

// messageText extracts text from a message whose Content may be a string or a
// []ContentPart (mirrors gateway.extractMessageContent).
func messageText(m *providers.Message) string {
	if s := m.GetContentString(); s != "" {
		return s
	}
	if parts, ok := m.Content.([]interface{}); ok {
		var texts []string
		for _, part := range parts {
			if pm, ok := part.(map[string]interface{}); ok {
				if t, ok := pm["text"].(string); ok {
					texts = append(texts, t)
				}
			}
		}
		return strings.Join(texts, "\n")
	}
	return ""
}

func promptWords(msgs []providers.Message) int {
	n := 0
	for i := range msgs {
		n += wordCount(messageText(&msgs[i]))
	}
	return n
}

func wordCount(s string) int {
	return len(strings.Fields(s))
}

// splitStream breaks reply text into word-sized pieces that reassemble (with
// normalized whitespace) into the original, simulating token-by-token output.
func splitStream(text string) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}
	pieces := make([]string, len(words))
	for i, word := range words {
		if i < len(words)-1 {
			pieces[i] = word + " "
		} else {
			pieces[i] = word
		}
	}
	return pieces
}
