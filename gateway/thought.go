package gateway

import (
	"regexp"
	"strings"

	"github.com/openziti/llm-gateway/providers"
)

// Google AI Studio's OpenAI-compatible endpoint wraps a "thinking" model's reasoning
// trace inline in the message content as a literal <thought>...</thought> block —
// there is no separate reasoning field to filter on, and no way to disable it for
// gemma-4-31b-it (its "thinking_config" is not adjustable). This strips that block so
// the tag never reaches API clients.

var thoughtBlockRe = regexp.MustCompile(`(?s)<thought>.*?</thought>`)

// stripThoughtBlock removes a <thought>...</thought> block from a non-streaming
// response's content.
func stripThoughtBlock(content string) string {
	return thoughtBlockRe.ReplaceAllString(content, "")
}

const thoughtOpenTag = "<thought>"
const thoughtCloseTag = "</thought>"

// thoughtStripper filters <thought>...</thought> out of a stream of content deltas.
// The block sits at the very start of the stream and its closing tag often shares a
// chunk with the start of the real answer (e.g. "</thought>Hello!"), so filtering
// needs state carried across chunks rather than a per-chunk regex.
type thoughtStripper struct {
	resolved  bool // decided this stream never had (or has finished) a thought block
	inThought bool
	buf       strings.Builder
}

// filter returns the portion of chunk that should reach the client, consuming any
// <thought>...</thought> prefix (partial or whole) it contains.
func (t *thoughtStripper) filter(chunk string) string {
	if t.resolved {
		return chunk
	}
	if !t.inThought {
		t.buf.WriteString(chunk)
		s := t.buf.String()
		if len(s) < len(thoughtOpenTag) && strings.HasPrefix(thoughtOpenTag, s) {
			return "" // still ambiguous — wait for more bytes
		}
		t.buf.Reset()
		if strings.HasPrefix(s, thoughtOpenTag) {
			t.inThought = true
			return t.filter(s[len(thoughtOpenTag):])
		}
		t.resolved = true
		return s
	}

	t.buf.WriteString(chunk)
	s := t.buf.String()
	if idx := strings.Index(s, thoughtCloseTag); idx >= 0 {
		t.inThought = false
		t.resolved = true
		t.buf.Reset()
		return s[idx+len(thoughtCloseTag):]
	}
	return "" // still inside the thought block — keep buffering, emit nothing
}

// allDeltasEmpty reports whether every choice in a stream chunk carries nothing
// worth forwarding: no content, no tool calls, no finish reason.
func allDeltasEmpty(choices []providers.Choice) bool {
	for _, c := range choices {
		if c.FinishReason != nil {
			return false
		}
		if c.Delta != nil && (c.Delta.Content != "" || len(c.Delta.ToolCalls) > 0) {
			return false
		}
	}
	return true
}
