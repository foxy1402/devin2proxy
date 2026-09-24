package devin

import (
	"context"
	"errors"
	"strings"
	"time"

	"devin2proxy/internal/pb"
)

// StatusPath is the RPC that reports the account behind a token: its identity,
// its plan and the quota counters. It is the same call `devin auth status` makes,
// reached with nothing but a session token. See docs/PROTOCOL.md.
const StatusPath = "/exa.seat_management_pb.SeatManagementService/GetUserStatus"

// statusTimeout bounds a status call. It is short because nothing waits on it:
// the pool fetches a status in the background, after a request has already
// failed, and gives up quietly if the answer is slow.
const statusTimeout = 20 * time.Second

// AccountStatus is what the status call tells the pool about one account.
//
// The plan block is where the quota lives, and its field names come from the CLI
// binary's own field table (the PlanStatus message): plan_end,
// available_flex_credits, used_flow_credits, used_prompt_credits, used_flex_credits,
// available_prompt_credits, available_flow_credits, top_up_status,
// was_reduced_by_orphaned_usage, grace_period_status, plan_start, grace_period_end,
// daily_quota_remaining_percent, weekly_quota_remaining_percent,
// overage_balance_micros, daily_quota_reset_at_unix, weekly_quota_reset_at_unix,
// acu_consumed, acu_limit — in that order. Which number is which was pinned by
// matching that order against the live values and against what the CLI's main page
// prints: it shows "41% remaining (reset in 3d 7h)" while field 15 reads 41 and
// field 18 is the epoch exactly 3d 7h out, so 15 is the weekly percentage and 18
// the weekly reset. The five consecutive fields therefore read, in the order the
// names appear: 14 daily percentage, 15 weekly percentage, 16 overage balance
// (signed), 17 daily reset, 18 weekly reset. See docs/PROTOCOL.md.
//
// Everything else stays in PlanFields keyed by field number, because the names
// above are not pinned to the remaining numbers (8 and 9 are non-zero on a free
// account and are some credit counter, but which one is not established).
type AccountStatus struct {
	Name       string `json:"name,omitempty"`
	Email      string `json:"email,omitempty"`
	UserID     string `json:"user_id,omitempty"`
	TeamID     string `json:"team_id,omitempty"`
	TeamStatus uint64 `json:"team_status,omitempty"`
	Plan       string `json:"plan,omitempty"`

	// Models is how many models the account is entitled to, and ModelList is the
	// same list with each model's details. The count is kept separate because it
	// is what a one-line summary wants, and because a caller that only cares
	// "is this account worth using" should not have to walk the list.
	Models    int         `json:"models,omitempty"`
	ModelList []ModelInfo `json:"model_list,omitempty"`

	// DailyRemainingPercent and WeeklyRemainingPercent are the plan block's fields
	// 14 and 15: the share of each quota window still available. They are pointers
	// because the two states that matter are indistinguishable as plain ints — a
	// window with nothing left reads 0, and a field the backend did not send reads
	// nothing at all, and only one of those is worth colouring red.
	DailyRemainingPercent  *int `json:"daily_quota_remaining_percent,omitempty"`
	WeeklyRemainingPercent *int `json:"weekly_quota_remaining_percent,omitempty"`

	// OverageBalanceMicros is plan field 16. It is signed, and a negative balance is
	// the ordinary reading, so the sign is kept rather than folded into a uint64.
	OverageBalanceMicros *int64 `json:"overage_balance_micros,omitempty"`

	// DailyReset and WeeklyReset are fields 17 and 18 of the plan block, the
	// times the daily and weekly quota refill. Established by observation: one
	// sample taken a day later had field 17 advanced by exactly 86400s while
	// field 18 had not moved, and field 18 matches the countdown the CLI prints.
	DailyReset  time.Time `json:"daily_reset,omitempty"`
	WeeklyReset time.Time `json:"weekly_reset,omitempty"`

	// PlanFields holds the plan block's remaining numeric fields by field number,
	// for the ones whose names are not established.
	PlanFields map[int]int64 `json:"plan_fields,omitempty"`
}

// ModelInfo is one model the account may use, as the status call reports it.
//
// UID is the identifier the backend's chat endpoint takes (chat_model_uid); the
// proxy drives its own coding models and does not switch to these, so the list is
// for showing what the account is entitled to rather than for routing.
type ModelInfo struct {
	Name      string `json:"name,omitempty"`
	UID       string `json:"uid,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Context   int64  `json:"context,omitempty"`
	MaxOutput int64  `json:"max_output,omitempty"`
	// Rates are the model's price rows: a label such as "Input", the unit it is
	// charged in, and any note the backend attaches to it.
	Rates []ModelRate `json:"rates,omitempty"`
}

// ModelRate is one row of a model's price list.
type ModelRate struct {
	Label string `json:"label,omitempty"`
	Unit  string `json:"unit,omitempty"`
	Note  string `json:"note,omitempty"`
}

// ResetDeadline returns the next reset time when the plan block says the account
// is out of quota, and the zero time when nothing suggests it is.
//
// The named percentages are read first, because they are the one measurement whose
// meaning is established: a window reporting 0% is empty, and its own reset is the
// one to wait for. A percentage that is *present* also settles the question when it
// is non-zero — it outranks the unnamed counters, which may be status enums that
// read zero on a perfectly healthy account, and benching a working account on one
// of those is the failure this function must not have. Only when no percentage came
// back at all does the older, blunter reading apply: a zero on any counter is
// treated as exhaustion.
//
// The interpretation stays blunt on purpose, and the answer is only ever used to
// *extend* a cooldown the backend already earned by refusing a request — never to
// create one. So the worst case is an account sitting out longer than it needed
// to, never a working account benched on a misread number.
func (s *AccountStatus) ResetDeadline(now time.Time) time.Time {
	if s == nil {
		return time.Time{}
	}
	var candidates []time.Time
	empty := false
	named := s.DailyRemainingPercent != nil || s.WeeklyRemainingPercent != nil
	if s.DailyRemainingPercent != nil && *s.DailyRemainingPercent == 0 {
		candidates = append(candidates, s.DailyReset)
		empty = true
	}
	if s.WeeklyRemainingPercent != nil && *s.WeeklyRemainingPercent == 0 {
		candidates = append(candidates, s.WeeklyReset)
		empty = true
	}
	if !named {
		for _, v := range s.PlanFields {
			if v == 0 {
				// The sooner of the two resets: waiting for the daily one is the
				// conservative choice when it is not clear which quota ran out.
				candidates = append(candidates, s.DailyReset, s.WeeklyReset)
				empty = true
				break
			}
		}
	}
	if !empty {
		return time.Time{}
	}
	if best := soonest(candidates, now); !best.IsZero() {
		return best
	}
	// Nothing failed and nothing is still ahead of us, which happens when a window
	// reads empty in a status taken before its own refill. Either window's next
	// refill is then the useful answer, which is the reading this function gave
	// before the percentages had names.
	return soonest([]time.Time{s.DailyReset, s.WeeklyReset}, now)
}

// soonest returns the earliest of the times that are still ahead of now, and the
// zero time when none is.
func soonest(times []time.Time, now time.Time) time.Time {
	var best time.Time
	for _, t := range times {
		if t.After(now) && (best.IsZero() || t.Before(best)) {
			best = t
		}
	}
	return best
}

// FetchAccountStatus asks the backend who this credential is and how much quota
// it has left. It is a plain unary call, so it spends no chat quota.
//
// Metadata is sent as this proxy's own identity rather than the CLI's "chisel".
// Both were checked against the live endpoint and the plan block is the same
// either way; the model list does differ between them, so what comes back here is
// the entitlement of this identity, not of the CLI. The account's plan, quota and
// identity — everything the pool acts on — are unaffected by the choice.
func (c *Client) FetchAccountStatus(ctx context.Context, creds *Credentials) (*AccountStatus, error) {
	if creds == nil || creds.APIKey == "" {
		return nil, errors.New("devin: no credential available")
	}
	body, err := c.PostUnary(ctx, creds, StatusPath, EncodeMetadataRequest(DefaultMetadata(creds.APIKey)))
	if err != nil {
		return nil, err
	}
	return DecodeAccountStatus(body), nil
}

// DecodeAccountStatus reads the fields of GetUserStatusResponse that have been
// confirmed against real responses.
//
// The live response wraps the account at field 1 (response.user_status) while the
// CLI's own cache stores that inner message on its own, so both shapes are
// accepted.
func DecodeAccountStatus(b []byte) *AccountStatus {
	s := &AccountStatus{}
	decodeStatusFields(b, s, 0)
	return s
}

func decodeStatusFields(b []byte, s *AccountStatus, depth int) {
	var inner []byte
	r := pb.NewReader(b)
	for {
		field, wire, ok := r.Field()
		if !ok {
			break
		}
		switch {
		case field == 1 && wire == pb.WireBytes:
			inner = r.Bytes()
		case field == 3 && wire == pb.WireBytes:
			s.Name = r.String()
		case field == 5 && wire == pb.WireBytes:
			s.TeamID = r.String()
		case field == 6 && wire == pb.WireVarint:
			s.TeamStatus = r.Uint64()
		case field == 7 && wire == pb.WireBytes:
			s.Email = r.String()
		case field == 13 && wire == pb.WireBytes:
			decodePlan(r.Bytes(), s)
		case field == 33 && wire == pb.WireBytes:
			decodeModelList(r.Bytes(), s)
		case field == 36 && wire == pb.WireBytes:
			s.UserID = r.String()
		default:
			r.Skip(wire)
		}
	}
	if depth == 0 && s.Email == "" && s.Name == "" && len(inner) > 0 {
		decodeStatusFields(inner, s, 1)
	}
}

// decodePlan reads the plan block: the plan's name at its nested field 1 → 2, the
// quota percentages and the two reset times, and every other integer kept by field
// number. The named fields are taken out of that catch-all so the map holds only
// the numbers nothing is known about.
func decodePlan(b []byte, s *AccountStatus) {
	r := pb.NewReader(b)
	for {
		field, wire, ok := r.Field()
		if !ok {
			return
		}
		switch {
		case field == 1 && wire == pb.WireBytes:
			inner := pb.NewReader(r.Bytes())
			for {
				f, w, ok := inner.Field()
				if !ok {
					break
				}
				if f == 2 && w == pb.WireBytes {
					s.Plan = inner.String()
					continue
				}
				inner.Skip(w)
			}
		case field == 14 && wire == pb.WireVarint:
			s.DailyRemainingPercent = intPtr(int(r.Uint64()))
		case field == 15 && wire == pb.WireVarint:
			s.WeeklyRemainingPercent = intPtr(int(r.Uint64()))
		case field == 16 && wire == pb.WireVarint:
			// Signed: the varint is ten bytes for a negative balance, and reading it
			// as unsigned is what turns -36850 into 18446744073709514766.
			s.OverageBalanceMicros = int64Ptr(int64(r.Uint64()))
		case field == 17 && wire == pb.WireVarint:
			s.DailyReset = time.Unix(int64(r.Uint64()), 0).UTC()
		case field == 18 && wire == pb.WireVarint:
			s.WeeklyReset = time.Unix(int64(r.Uint64()), 0).UTC()
		case wire == pb.WireVarint:
			if s.PlanFields == nil {
				s.PlanFields = map[int]int64{}
			}
			s.PlanFields[field] = int64(r.Uint64())
		default:
			r.Skip(wire)
		}
	}
}

func intPtr(v int) *int { return &v }

func int64Ptr(v int64) *int64 { return &v }

// decodeModelList reads field 33, the account's model list: a wrapper whose
// repeated field 1 is one model each, plus grouping messages at field 2 that say
// which provider each model belongs to. Every number and name in here was read
// off real responses; see the field table in docs/PROTOCOL.md.
func decodeModelList(b []byte, s *AccountStatus) {
	var list []ModelInfo
	grouped := map[string]string{}
	r := pb.NewReader(b)
	for {
		field, wire, ok := r.Field()
		if !ok {
			break
		}
		switch {
		case field == 1 && wire == pb.WireBytes:
			list = append(list, decodeModelEntry(r.Bytes()))
		case field == 2 && wire == pb.WireBytes:
			// A grouping message. The provider one is the only group worth keeping:
			// the cost group only repeats the models' own rate rows.
			readModelGroup(r.Bytes(), grouped)
		default:
			r.Skip(wire)
		}
	}
	for i := range list {
		list[i].Provider = grouped[list[i].Name]
	}
	s.ModelList = list
	s.Models = len(list)
}

// decodeModelEntry reads one model: the display name at field 1, its uid at
// field 22 (repeated as field 17 of the spec block, which is used when field 22
// is absent), the context/output limits in the spec block, and the rate rows.
func decodeModelEntry(b []byte) ModelInfo {
	var m ModelInfo
	r := pb.NewReader(b)
	for {
		field, wire, ok := r.Field()
		if !ok {
			return m
		}
		switch {
		case field == 1 && wire == pb.WireBytes:
			m.Name = r.String()
		case field == 22 && wire == pb.WireBytes:
			m.UID = r.String()
		case field == 23 && wire == pb.WireBytes:
			decodeModelSpec(r.Bytes(), &m)
		case field == 32 && wire == pb.WireBytes:
			m.Rates = append(m.Rates, decodeModelRate(r.Bytes()))
		default:
			r.Skip(wire)
		}
	}
}

// decodeModelSpec reads the model's limits. Only the two token counts are named:
// field 4 is the context window and field 13 the maximum output, both confirmed
// against the models' published numbers. The rest (a numeric model id, a
// tokenizer name, the api server the model is served from) are not shown.
func decodeModelSpec(b []byte, m *ModelInfo) {
	r := pb.NewReader(b)
	for {
		field, wire, ok := r.Field()
		if !ok {
			return
		}
		switch {
		case field == 4 && wire == pb.WireVarint:
			m.Context = int64(r.Uint64())
		case field == 13 && wire == pb.WireVarint:
			m.MaxOutput = int64(r.Uint64())
		case field == 17 && wire == pb.WireBytes:
			if uid := r.String(); m.UID == "" {
				m.UID = uid
			}
		default:
			r.Skip(wire)
		}
	}
}

// decodeModelRate reads one price row: its label at field 1, the unit at field 3
// and the note at field 7.
func decodeModelRate(b []byte) ModelRate {
	var rate ModelRate
	r := pb.NewReader(b)
	for {
		field, wire, ok := r.Field()
		if !ok {
			return rate
		}
		switch {
		case field == 1 && wire == pb.WireBytes:
			rate.Label = r.String()
		case field == 3 && wire == pb.WireBytes:
			rate.Unit = r.String()
		case field == 7 && wire == pb.WireBytes:
			rate.Note = r.String()
		default:
			r.Skip(wire)
		}
	}
}

// readModelGroup fills name→provider from a grouping message. The message is
// {1: heading, 2: repeated {1: provider, 2: repeated model names}}, and the
// heading is what says the group is the provider one: the sibling group is
// "Cost" and carries no provider at all.
func readModelGroup(b []byte, out map[string]string) {
	var heading string
	var groups [][]byte
	r := pb.NewReader(b)
	for {
		field, wire, ok := r.Field()
		if !ok {
			break
		}
		switch {
		case field == 1 && wire == pb.WireBytes:
			heading = r.String()
		case field == 2 && wire == pb.WireBytes:
			groups = append(groups, r.Bytes())
		default:
			r.Skip(wire)
		}
	}
	if !strings.EqualFold(strings.TrimSpace(heading), "provider") {
		return
	}
	for _, g := range groups {
		var provider string
		var names []string
		gr := pb.NewReader(g)
		for {
			field, wire, ok := gr.Field()
			if !ok {
				break
			}
			switch {
			case field == 1 && wire == pb.WireBytes:
				provider = gr.String()
			case field == 2 && wire == pb.WireBytes:
				names = append(names, gr.String())
			default:
				gr.Skip(wire)
			}
		}
		if provider == "" {
			continue
		}
		for _, n := range names {
			out[n] = provider
		}
	}
}

// String renders the account for a log line. It never includes the token.
func (s *AccountStatus) String() string {
	if s == nil {
		return "unknown account"
	}
	switch {
	case s.Email != "":
		return s.Email
	case s.UserID != "":
		return s.UserID
	default:
		return "unnamed account"
	}
}
