package gateway

import "testing"

func TestStripThoughtBlock(t *testing.T) {
	got := stripThoughtBlock("<thought>reasoning here</thought>Hello!")
	if got != "Hello!" {
		t.Errorf("got %q, want %q", got, "Hello!")
	}
	if got := stripThoughtBlock("no thought tag"); got != "no thought tag" {
		t.Errorf("got %q, want unchanged", got)
	}
}

func TestThoughtStripperFiltersAcrossChunks(t *testing.T) {
	ts := &thoughtStripper{}
	chunks := []string{
		"<thought>The user said ",
		"\"hii\".\nThis is a greeting.\n",
		"</thought>Hello! How can I help",
		" you today?",
	}
	var out string
	for _, c := range chunks {
		out += ts.filter(c)
	}
	if out != "Hello! How can I help you today?" {
		t.Errorf("got %q", out)
	}
}

func TestThoughtStripperSplitOpenTagAcrossChunks(t *testing.T) {
	ts := &thoughtStripper{}
	chunks := []string{"<thou", "ght>reasoning</though", "t>answer"}
	var out string
	for _, c := range chunks {
		out += ts.filter(c)
	}
	if out != "answer" {
		t.Errorf("got %q", out)
	}
}

func TestThoughtStripperPassthroughWhenNoThought(t *testing.T) {
	ts := &thoughtStripper{}
	chunks := []string{"Hello", " there", "!"}
	var out string
	for _, c := range chunks {
		out += ts.filter(c)
	}
	if out != "Hello there!" {
		t.Errorf("got %q", out)
	}
}

func TestThoughtStripperShortNonThoughtPrefix(t *testing.T) {
	ts := &thoughtStripper{}
	// "<th" is a genuine prefix of "<thought>" but the stream never completes it —
	// it must still flush out once the divergence is unambiguous.
	got := ts.filter("<th") + ts.filter("is is fine")
	if got != "<this is fine" {
		t.Errorf("got %q", got)
	}
}
