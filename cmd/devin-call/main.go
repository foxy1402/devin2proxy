// Command devin-call issues one live GetChatMessage request built entirely by
// this package, to prove a synthetic payload is accepted by the backend.
//
// It is the experiment that answers whether the cascade/trajectory ids must be
// minted by the CLI or whether freshly generated UUIDs work.
//
//	go run ./cmd/devin-call -prompt "say hi"
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"devin2proxy/internal/devin"
)

func main() {
	prompt := flag.String("prompt", "Reply with exactly the word: pong", "user message")
	system := flag.String("system", "You are a helpful assistant. Keep answers short.", "system prompt")
	model := flag.String("model", devin.DefaultModel, "model uid")
	cascade := flag.String("cascade", "", "cascade id to send; empty omits it (the backend then starts a fresh trajectory), \"random\" generates one")
	messageID := flag.String("message-id", "", "user message id; default is a fresh UUID")
	maxTokens := flag.Uint64("max-tokens", 128000, "max_tokens")
	temperature := flag.Float64("temperature", 1.0, "temperature")
	topP := flag.Float64("top-p", 0.95, "top_p")
	topK := flag.Uint64("top-k", 40, "top_k")
	maxNewlines := flag.Uint64("max-newlines", 400, "max_newlines")
	showThinking := flag.Bool("thinking", true, "print thinking deltas")
	timeout := flag.Duration("timeout", 3*time.Minute, "overall timeout")

	replay := flag.String("replay", "", "replay a captured request body verbatim (path to a devin-probe capture)")
	dropFields := flag.String("drop", "", "with -replay: comma-separated top-level request field numbers to remove")
	dropMeta := flag.String("drop-md", "", "with -replay: comma-separated Metadata field numbers to remove")
	quiet := flag.Bool("quiet", false, "suppress the request dump, leaving only the outcome")
	reencode := flag.Bool("reencode", false, "with -replay: decode, apply -trajectory/-cascade overrides, then re-encode with our own encoder")
	trajectory := flag.String("trajectory", "", "trajectory id to set: empty leaves it, \"off\" removes it, \"random\" generates a fresh one")
	noTrajectory := flag.Bool("no-trajectory", false, "omit trajectory_reference from a from-scratch request")
	turns := flag.String("turns", "", "multi-turn probe: semicolon-separated role:text pairs, e.g. \"user:hi;assistant:yo;user:bye\"")
	asrc := flag.Int("asrc", devin.SourceUser, "source value used for assistant turns in -turns")
	athink := flag.String("athink", "", "thinking text attached to assistant turns in -turns")
	asig := flag.String("asig", "", "signature attached to assistant turns in -turns")
	amidbot := flag.Bool("amidbot", false, "prefix assistant turn message ids with \"bot-\" (the form the server mints)")
	reqType := flag.Int("reqtype", devin.RequestTypeCascade, "request_type value")
	planner := flag.Int("planner", devin.PlannerModeDefault, "planner_mode value; -1 omits the field")
	flag.Parse()

	creds, err := devin.LoadCredentials()
	if err != nil {
		fatal(err)
	}
	fmt.Printf("credential  : %s\n", mask(creds.APIKey))
	fmt.Printf("backend     : %s\n", creds.APIServerURL)

	cascadeID := *cascade
	if cascadeID == "random" {
		cascadeID = devin.MustUUID()
	}
	msgID := *messageID
	if msgID == "" {
		msgID = devin.MustUUID()
	}
	if cascadeID == "" {
		fmt.Printf("cascade_id  : <omitted>\n")
	} else {
		fmt.Printf("cascade_id  : %s\n", cascadeID)
	}
	fmt.Printf("message_id  : %s\n", msgID)
	fmt.Printf("model       : %s\n\n", *model)

	client := devin.NewClient(devin.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	var stream *devin.Stream

	if *replay != "" {
		payload, err := loadReplay(*replay, *dropFields, *dropMeta)
		if err != nil {
			fatal(err)
		}
		if *reencode {
			before := len(payload)
			payload, err = reencodeRequest(payload, *trajectory, *cascade)
			if err != nil {
				fatal(err)
			}
			fmt.Printf("re-encoded  : %d -> %d bytes (unknown fields are not carried over)\n", before, len(payload))
		}
		fmt.Printf("replaying   : %s (%d byte payload)\n\n", *replay, len(payload))
		if !*quiet {
			fmt.Println("--- request to send ---")
			fmt.Print(devin.DescribeRequest(devin.DecodeGetChatMessageRequest(payload)))
			fmt.Println()
		}
		stream, err = client.PostStreamRaw(ctx, creds, payload)
		if err != nil {
			fatal(err)
		}
	} else {
		prompts, err := buildTurns(*turns, *prompt, msgID, *asrc, *athink, *asig, *amidbot)
		if err != nil {
			fatal(err)
		}
		req := &devin.GetChatMessageRequest{
			Metadata:           devin.DefaultMetadata(creds.APIKey),
			Prompt:             *system,
			ChatMessagePrompts: prompts,
			RequestType:        *reqType,
			Configuration: &devin.CompletionConfiguration{
				NumCompletions: 1,
				MaxTokens:      *maxTokens,
				Temperature:    *temperature,
				TopP:           *topP,
				TopK:           *topK,
				MaxNewlines:    *maxNewlines,
			},
			CascadeID:    cascadeID,
			ChatModelUID: *model,
		}
		// A negative value means "leave the field out"; proto3 omits zero, so any
		// value <= 0 is equivalent to omitting it.
		if *planner > 0 {
			req.PlannerMode = *planner
		}
		if !*noTrajectory {
			req.TrajectoryRef = devin.NewTrajectoryReference()
		}
		if !*quiet {
			fmt.Println("--- request as built ---")
			fmt.Print(devin.DescribeRequest(req))
			fmt.Println()
		}
		stream, err = client.GetChatMessage(ctx, creds, req)
		if err != nil {
			fatal(err)
		}
	}
	defer stream.Close()

	fmt.Println("--- response stream ---")
	var frames int
	var text, thinking string
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			fmt.Printf("\n[error] %v\n", err)
			fmt.Printf("frames received before error: %d\n", frames)
			os.Exit(1)
		}
		frames++

		if chunk.MessageID != "" && frames == 1 {
			fmt.Printf("[meta] message_id=%s request_id=%s\n", chunk.MessageID, chunk.RequestID)
		}
		if chunk.DeltaThinking != "" {
			thinking += chunk.DeltaThinking
			if *showThinking {
				fmt.Printf("[think] %q\n", chunk.DeltaThinking)
			}
		}
		if chunk.DeltaText != "" {
			text += chunk.DeltaText
			fmt.Printf("[text] %q\n", chunk.DeltaText)
		}
		if chunk.StopReasonSet {
			fmt.Printf("[stop] %s\n", devin.StopReasonName(chunk.StopReason))
		}
		if u := chunk.Usage; u != nil && (u.InputTokens != 0 || u.OutputTokens != 0) {
			fmt.Printf("[usage] in=%d out=%d cache_read=%d cache_write=%d model=%s\n",
				u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.ModelUID)
		}
		for _, tc := range chunk.DeltaToolCalls {
			fmt.Printf("[tool] id=%q name=%q args=%q\n", tc.ID, tc.Name, tc.ArgumentsJSON)
		}
		if len(chunk.Unknown) > 0 {
			fmt.Printf("[unknown] %+v\n", chunk.Unknown)
		}
	}

	fmt.Printf("\n--- done: %d frames ---\n", frames)
	fmt.Printf("thinking (%d bytes):\n%s\n", len(thinking), thinking)
	fmt.Printf("text (%d bytes):\n%s\n", len(text), text)
}

// buildTurns assembles the chat_message_prompts list. Without -turns it
// reproduces the single-user-turn shape the CLI sends; with -turns it emits one
// prompt per role:text pair, which is how the source value the backend expects
// for an assistant turn was determined.
func buildTurns(spec, prompt, msgID string, asrc int, athink, asig string, amidbot bool) ([]devin.ChatMessagePrompt, error) {
	if spec == "" {
		return []devin.ChatMessagePrompt{{
			MessageID: msgID,
			Source:    devin.SourceUser,
			Prompt:    prompt,
		}}, nil
	}

	var out []devin.ChatMessagePrompt
	for _, part := range strings.Split(spec, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		role, text, ok := strings.Cut(part, ":")
		if !ok {
			return nil, fmt.Errorf("bad turn %q: want role:text", part)
		}
		role = strings.ToLower(strings.TrimSpace(role))

		p := devin.ChatMessagePrompt{MessageID: devin.MustUUID(), Prompt: text}
		switch role {
		case "user":
			p.Source = devin.SourceUser
		case "system":
			p.Source = devin.SourceSystem
		case "assistant":
			p.Source = asrc
			if amidbot {
				p.MessageID = "bot-" + devin.MustUUID()
			}
			if athink != "" {
				p.Thinking = athink
			}
			if asig != "" {
				p.Signature = asig
			}
		case "tool":
			p.Source = devin.SourceTool
		default:
			return nil, fmt.Errorf("bad role %q in turn %q", role, part)
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("-turns produced no prompts")
	}
	return out, nil
}

func mask(s string) string {
	if len(s) <= 24 {
		return s
	}
	return fmt.Sprintf("%s...(len %d)", s[:24], len(s))
}

// loadReplay reads a captured request and optionally strips fields, so the
// backend's actual requirements can be found by removing one field at a time
// from a request that is known to work.
func loadReplay(path, dropFields, dropMeta string) ([]byte, error) {
	body, err := devin.ReadCaptureBody(path)
	if err != nil {
		return nil, err
	}
	payload, err := devin.RequestPayload(body)
	if err != nil {
		return nil, err
	}
	if dropFields != "" {
		set, err := parseIntSet(dropFields)
		if err != nil {
			return nil, err
		}
		payload, err = devin.FilterFields(payload, set)
		if err != nil {
			return nil, err
		}
	}
	if dropMeta != "" {
		set, err := parseIntSet(dropMeta)
		if err != nil {
			return nil, err
		}
		// Field 1 of a GetChatMessageRequest is the Metadata sub-message.
		payload, err = devin.FilterSubMessage(payload, 1, set)
		if err != nil {
			return nil, err
		}
	}
	return payload, nil
}

func parseIntSet(s string) (map[int]bool, error) {
	out := map[int]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("bad field number %q: %w", part, err)
		}
		out[n] = true
	}
	return out, nil
}

// reencodeRequest decodes a captured request, applies the requested identity
// overrides, and serialises it again with our own encoder. Running this on a
// request known to work is what separates an encoder bug from a field-value
// problem.
func reencodeRequest(payload []byte, trajectory, cascade string) ([]byte, error) {
	req := devin.DecodeGetChatMessageRequest(payload)

	switch trajectory {
	case "":
		// leave whatever the capture contained
	case "off":
		req.TrajectoryRef = nil
	case "random":
		id, err := devin.NewUUID()
		if err != nil {
			return nil, err
		}
		req.TrajectoryRef = &devin.TrajectoryReference{TrajectoryID: id, TrajectoryType: 4, Field4: 14}
	default:
		if req.TrajectoryRef == nil {
			req.TrajectoryRef = &devin.TrajectoryReference{TrajectoryType: 4, Field4: 14}
		}
		req.TrajectoryRef.TrajectoryID = trajectory
	}

	switch cascade {
	case "":
		// leave as captured
	case "off":
		req.CascadeID = ""
	case "random":
		id, err := devin.NewUUID()
		if err != nil {
			return nil, err
		}
		req.CascadeID = id
	default:
		req.CascadeID = cascade
	}

	return devin.EncodeGetChatMessageRequest(req), nil
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "FAILED: %v\n", err)
	os.Exit(1)
}
