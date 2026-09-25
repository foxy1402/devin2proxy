package devin

import (
	"strings"
	"testing"
)

// The shape line is the diff an operator reads when the backend refuses a
// client request that the probe served: every count on it has to be one the
// request actually carries, and the sampling fields must always appear,
// because a present-but-zero field is a rejection in its own right.
func TestShapeLineReportsTheCountsThatDecideServing(t *testing.T) {
	req := &GetChatMessageRequest{
		Metadata:    &Metadata{},
		Prompt:      "system prompt text",
		RequestType: RequestTypeCascade,
		ChatMessagePrompts: []ChatMessagePrompt{
			{Source: SourceUser, Prompt: "hello"},
			{Source: SourceUser, Prompt: "again", Images: []ImageData{{}}},
			{Source: SourceTool, Prompt: "tool result"},
		},
		Configuration: &CompletionConfiguration{
			MaxTokens:   8192,
			Temperature: 1,
			TopP:        0.95,
			TopK:        40,
			MaxNewlines: 400,
		},
		Tools: []ChatToolDefinition{
			{Name: "a"}, {Name: "b"}, {Name: "c"},
		},
		PlannerMode:  PlannerModeDefault,
		ChatModelUID: "swe-1-6-slow",
	}
	line := req.ShapeLine()
	want := "model=swe-1-6-slow rtype=5 planner=1 prompts=3(user=2 system=0 tool=1) tools=3 images=1 turn_bytes=21 system_bytes=18" +
		" max_tokens=8192 temp=1 top_p=0.95 top_k=40 max_newlines=400"
	if line != want {
		t.Errorf("ShapeLine:\n got %q\nwant %q", line, want)
	}

	// A request built without a Configuration block says so instead of
	// printing zeros that look like the real path's values.
	bare := &GetChatMessageRequest{Prompt: "x"}
	if !strings.Contains(bare.ShapeLine(), "config=absent") {
		t.Errorf("a request with no configuration printed %q, want a config=absent marker", bare.ShapeLine())
	}
	if got := (*GetChatMessageRequest)(nil).ShapeLine(); got != "<nil request>" {
		t.Errorf("nil request printed %q", got)
	}
}
