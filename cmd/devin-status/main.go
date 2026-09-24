// Command devin-status asks the backend for the signed-in account's own status —
// the email, name, plan, team and quota counters behind `devin auth status`, but
// called directly instead of through the CLI.
//
//	exa.seat_management_pb.SeatManagementService/GetUserStatus
//
// That is what makes a multi-account dashboard possible without asking the
// operator to keep switching the CLI's own login: the endpoint is reached with
// nothing but the session token, so a stored token can be labelled and its quota
// read at any time.
//
//	go run ./cmd/devin-status                 # the stored credential
//	go run ./cmd/devin-status -dump           # plus the raw wire dump
//	go run ./cmd/devin-status -tokens-file bin\tokens.txt
//
// It is a plain unary call, so it costs no chat quota.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"devin2proxy/internal/devin"
	"devin2proxy/internal/pb"
)

// userStatusPath is the RPC behind `devin auth status`, recovered from the CLI
// binary. The service prefix is what the backend routes on; the path is exact.
const userStatusPath = "/exa.seat_management_pb.SeatManagementService/GetUserStatus"

func main() {
	token := flag.String("token", "", "query this session token instead of the stored credential")
	tokensFile := flag.String("tokens-file", "", "query every token in this file, one per line")
	path := flag.String("path", userStatusPath, "override the RPC path")
	dump := flag.Bool("dump", false, "print the raw wire dump of each response")
	save := flag.String("save", "", "write the first response body to this file, as a fixture for the offline quota probe")
	cache := flag.String("cache", "", "read a CLI cache file (user_status.<digest>.bin) instead of calling the backend")
	codec := flag.String("codec", "proto", "wire encoding to use: proto or json")
	// The CLI reports itself as "chisel", and the backend answers that identity
	// with the account's full model list — 251 entries against 4 for this proxy's
	// own "devin-cli" identity. A status tool should see what the CLI sees, so
	// that is the default here.
	ideName := flag.String("ide-name", "chisel", "metadata.ide_name; \"chisel\" is what the CLI sends, and the model list depends on it")
	timeout := flag.Duration("timeout", 30*time.Second, "per-request timeout")
	flag.Parse()

	if *cache != "" {
		if err := readCache(*cache, *dump); err != nil {
			fatal(err)
		}
		return
	}

	creds, err := collect(*token, *tokensFile)
	if err != nil {
		fatal(err)
	}
	if len(creds) == 0 {
		fatal(fmt.Errorf("no credential: pass -token, -tokens-file, or run `devin auth login`"))
	}

	if *codec != "proto" {
		// Connect's JSON codec is not served for this RPC: a JSON body is refused
		// with the same invalid_argument an empty one gets, in both snake_case and
		// the lowerCamelCase Connect's own spec prescribes. Protobuf it is.
		fatal(fmt.Errorf("the seat-management RPCs only answer on the proto codec, so -codec must be proto"))
	}

	client := devin.NewClient(devin.Options{MaxConcurrent: 1, HeaderTimeout: *timeout})
	failed := false
	saved := false
	for i, c := range creds {
		if len(creds) > 1 {
			fmt.Printf("=== %d/%d  %s ===\n", i+1, len(creds), c.Source)
		}
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		md := devin.DefaultMetadata(c.APIKey)
		if *ideName != "" {
			md.IDEName = *ideName
		}
		body, err := client.PostUnary(ctx, c, *path, devin.EncodeMetadataRequest(md))
		cancel()
		if err != nil {
			failed = true
			fmt.Printf("  error: %v\n\n", err)
			continue
		}
		if *save != "" && !saved {
			// A fixture so tools/quota-probe.mjs can answer the pool's status call
			// with a real response. It names the account, so it belongs in bin/ with
			// the other credential-bearing files, never in the repository.
			if err := os.WriteFile(*save, body, 0o600); err != nil {
				fatal(err)
			}
			fmt.Printf("  saved %d bytes to %s\n", len(body), *save)
			saved = true
		}
		printStatus(body, *dump)
	}
	if failed {
		os.Exit(1)
	}
}

// cacheEnvelope is the wrapper the CLI stores its cached RPC responses in. The
// identity_digest is a hash of the credential it belongs to — which is also the
// suffix of the file name, so one account's status can never be read as
// another's.
type cacheEnvelope struct {
	Version        int    `json:"version"`
	IdentityDigest string `json:"identity_digest"`
	FetchedAtSecs  int64  `json:"fetched_at_secs"`
	Payload        string `json:"payload"`
}

// readCache prints what the CLI cached for this account, which is the same
// message the endpoint returns. It is the offline way to see the schema.
func readCache(path string, dump bool) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var env cacheEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	payload, err := base64.StdEncoding.DecodeString(env.Payload)
	if err != nil {
		return fmt.Errorf("decode payload in %s: %w", path, err)
	}

	fmt.Printf("file            %s\n", filepath.Base(path))
	fmt.Printf("identity digest %s\n", env.IdentityDigest)
	fmt.Printf("fetched at      %s\n\n", time.Unix(env.FetchedAtSecs, 0).UTC().Format(time.RFC3339))
	printStatus(payload, dump)
	return nil
}

// collect assembles the credentials to query. An empty result means the caller
// should fall back to the stored one, so a bare `-token ""` is not a hard error.
func collect(token, tokensFile string) ([]*devin.Credentials, error) {
	var tokens []string
	if token != "" {
		tokens = append(tokens, token)
	}
	if tokensFile != "" {
		fromFile, err := devin.TokensFromFile(tokensFile)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, fromFile...)
	}
	if len(tokens) > 0 {
		pool := devin.NewPool(tokens, "")
		var out []*devin.Credentials
		for {
			c := pool.Next()
			if c == nil || len(out) >= pool.Len() {
				break
			}
			out = append(out, c)
		}
		return out, nil
	}

	c, err := devin.LoadCredentials()
	if err != nil {
		return nil, err
	}
	return []*devin.Credentials{c}, nil
}

// printStatus renders one response. The decoding lives in internal/devin, beside
// the pool that acts on it, so this tool cannot drift from what the proxy does
// with the same bytes; only the presentation is local, since an operator reading
// the output wants more than the single line a log gets.
func printStatus(body []byte, dump bool) {
	s := devin.DecodeAccountStatus(body)

	rows := [][2]string{
		{"email", s.Email},
		{"name", s.Name},
		{"user id", s.UserID},
		{"team id", s.TeamID},
		{"team status", statusName(s.TeamStatus)},
		{"plan", s.Plan},
		{"models", fmt.Sprintf("%d listed", s.Models)},
		{"quota left", quotaSummary(s)},
	}
	for _, r := range rows {
		if r[1] != "" {
			fmt.Printf("  %-12s %s\n", r[0], r[1])
		}
	}
	// The two quota windows, in the shape the CLI's own main page uses: a
	// percentage and how long until that window refills.
	for _, w := range []struct {
		label   string
		percent *int
		reset   time.Time
	}{
		{"quota week", s.WeeklyRemainingPercent, s.WeeklyReset},
		{"quota today", s.DailyRemainingPercent, s.DailyReset},
	} {
		if w.percent == nil {
			continue
		}
		fmt.Printf("  %-12s %d%% remaining%s\n", w.label, *w.percent, untilSuffix(w.reset))
	}
	if s.OverageBalanceMicros != nil {
		fmt.Printf("  %-12s %d micros\n", "overage", *s.OverageBalanceMicros)
	}
	if !s.DailyReset.IsZero() {
		fmt.Printf("  %-12s %s\n", "daily reset", stamp(s.DailyReset))
	}
	if !s.WeeklyReset.IsZero() {
		fmt.Printf("  %-12s %s\n", "weekly reset", stamp(s.WeeklyReset))
	}
	if len(s.PlanFields) > 0 {
		keys := make([]int, 0, len(s.PlanFields))
		for k := range s.PlanFields {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%d=%d", k, s.PlanFields[k]))
		}
		fmt.Printf("  %-12s %s\n", "plan fields", strings.Join(parts, " "))
	}

	if dump {
		fmt.Println("  --- wire ---")
		describe(payloadOrBody(body), 0, "  ")
	}
	fmt.Println()
}

// quotaSummary states what the pool would do with this account right now, which is
// the question an operator actually has: will this token keep serving requests.
func quotaSummary(s *devin.AccountStatus) string {
	if deadline := s.ResetDeadline(time.Now()); !deadline.IsZero() {
		return fmt.Sprintf("out of quota; held out until %s", deadline.Format(time.RFC3339))
	}
	if s.WeeklyRemainingPercent != nil || s.DailyRemainingPercent != nil {
		return "some left (no quota window reads 0%)"
	}
	if len(s.PlanFields) == 0 {
		return "unknown (no counters on the plan block)"
	}
	return "some left (no counter reads zero)"
}

func stamp(t time.Time) string {
	return t.Format(time.RFC3339) + fmt.Sprintf(" (%d)", t.Unix())
}

// untilSuffix renders " (resets in 3d 7h)" for a reset still ahead, and "" when
// there is no reset to speak of. The countdown is what makes a percentage mean
// something: 0% a minute before the window refills is not the same as 0% with
// three days to go.
func untilSuffix(reset time.Time) string {
	if reset.IsZero() {
		return ""
	}
	left := time.Until(reset)
	if left <= 0 {
		return " (reset has passed)"
	}
	left = left.Round(time.Minute)
	days := int(left.Hours()) / 24
	hours := int(left.Hours()) % 24
	mins := int(left.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf(" (resets in %dd %dh)", days, hours)
	case hours > 0:
		return fmt.Sprintf(" (resets in %dh %dm)", hours, mins)
	default:
		return fmt.Sprintf(" (resets in %dm)", mins)
	}
}

// statusName renders the team-membership enum. 2 is what an approved account
// reports; the other values are left as numbers because only this one has been
// seen.
func statusName(v uint64) string {
	if v == 2 {
		return "2 (approved)"
	}
	if v == 0 {
		return ""
	}
	return fmt.Sprintf("%d", v)
}

// payloadOrBody tolerates a Connect-enveloped body as well as a bare message.
func payloadOrBody(b []byte) []byte {
	if payload, err := devin.RequestPayload(b); err == nil {
		return payload
	}
	return b
}

// describe prints the structure of a protobuf message to a bounded depth, which
// is how the field numbers in this file were read off real responses.
func describe(b []byte, depth int, indent string) {
	if depth > 2 {
		return
	}
	r := pb.NewReader(b)
	for {
		field, wire, ok := r.Field()
		if !ok {
			return
		}
		switch wire {
		case pb.WireVarint:
			fmt.Printf("%s%d: varint %d\n", indent, field, r.Uint64())
		case pb.WireFixed32:
			fmt.Printf("%s%d: fixed32 %d\n", indent, field, r.Fixed32())
		case pb.WireBytes:
			raw := r.Bytes()
			if looksLikeText(raw) {
				fmt.Printf("%s%d: %q\n", indent, field, truncate(string(raw), 80))
				continue
			}
			fmt.Printf("%s%d: message %d bytes\n", indent, field, len(raw))
			describe(raw, depth+1, indent+"  ")
		default:
			r.Skip(wire)
		}
	}
}

func looksLikeText(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	for _, c := range b {
		if c < 0x20 && c != '\t' && c != '\n' && c != '\r' {
			return false
		}
		if c > 0x7e {
			return false
		}
	}
	return true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "devin-status:", err)
	os.Exit(2)
}
