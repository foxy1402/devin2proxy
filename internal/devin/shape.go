package devin

import (
	"fmt"
	"strings"
)

// RequestShapeLogger, when non-nil, receives every translated backend request
// just before it is sent — from the OpenAI relay paths and from the dashboard's
// probe alike. main wires it from DEVIN2PROXY_DEBUG_SHAPE; nil is the default
// and nothing is logged.
//
// It exists because a client-side refusal ("an internal error occurred" with a
// trace ID) says nothing about what was refused, and the dashboard's own probe
// serving the same account seconds earlier proves the difference is in the
// request shape. With the hook on, a failing client request and a passing Test
// print their shapes into the same log, and the diff is the diagnosis.
var RequestShapeLogger func(*GetChatMessageRequest)

// EmitRequestShape hands a built request to the logger when one is installed.
func EmitRequestShape(req *GetChatMessageRequest) {
	if RequestShapeLogger != nil {
		RequestShapeLogger(req)
	}
}

// ShapeLine renders the request as one line of the counts and sizes that
// decide whether this backend serves it or refuses it: the model, the prompt
// split, the tool and image counts, and every sampling field (a present-but-
// zero sampling field is a rejection, which is why they are printed even when
// they look boring).
func (r *GetChatMessageRequest) ShapeLine() string {
	if r == nil {
		return "<nil request>"
	}
	var user, system, tool int
	promptBytes := 0
	images := 0
	for _, p := range r.ChatMessagePrompts {
		switch p.Source {
		case SourceUser:
			user++
		case SourceSystem:
			system++
		case SourceTool:
			tool++
		}
		promptBytes += len(p.Prompt)
		images += len(p.Images)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "model=%s rtype=%d planner=%d prompts=%d(user=%d system=%d tool=%d) tools=%d images=%d turn_bytes=%d system_bytes=%d",
		r.ChatModelUID, r.RequestType, r.PlannerMode,
		len(r.ChatMessagePrompts), user, system, tool,
		len(r.Tools), images, promptBytes, len(r.Prompt))
	if c := r.Configuration; c != nil {
		fmt.Fprintf(&b, " max_tokens=%d temp=%.3g top_p=%.3g top_k=%d max_newlines=%d",
			c.MaxTokens, c.Temperature, c.TopP, c.TopK, c.MaxNewlines)
	} else {
		b.WriteString(" config=absent")
	}
	return b.String()
}
