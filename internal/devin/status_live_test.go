package devin

import (
	"context"
	"os"
	"testing"
	"time"
)

// The unit tests decode responses this package built itself, which cannot catch a
// request shape or field number that was misread from the captured bytes. This one
// closes that gap by asking the real endpoint with the real client path the pool
// uses:
//
//	set DEVIN2PROXY_TEST_ACCOUNT=1
//	go test ./internal/devin -run TestLiveAccountStatus -v
//
// It reads the stored CLI credential, so it spends one unary seat-management call
// and no chat quota.
const liveAccountEnv = "DEVIN2PROXY_TEST_ACCOUNT"

func TestLiveAccountStatus(t *testing.T) {
	if os.Getenv(liveAccountEnv) == "" {
		t.Skipf("set %s=1 to query the real endpoint with the stored credential", liveAccountEnv)
	}
	creds, err := LoadCredentials()
	if err != nil {
		t.Skipf("no usable stored credential: %v", err)
	}

	client := NewClient(Options{MaxConcurrent: 1, HeaderTimeout: 30 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st, err := client.FetchAccountStatus(ctx, creds)
	if err != nil {
		t.Fatalf("FetchAccountStatus: %v", err)
	}

	// Structural facts, not values: an account that answered at all must have
	// identified itself, and the fields the pool reasons about must have decoded.
	// Asserting the plan name or a counter would break on the next billing change
	// without saying anything about this code.
	if st.Email == "" && st.UserID == "" {
		t.Error("no identity decoded; the response shape has changed")
	}
	// Something the pool can reason about must have decoded: either a named quota
	// window or one of the unnamed counters. Which of the two a given plan returns is
	// the backend's business, so neither alone is required.
	if st.DailyRemainingPercent == nil && st.WeeklyRemainingPercent == nil && len(st.PlanFields) == 0 {
		t.Error("no quota figures decoded at all; ResetDeadline can never fire")
	}
	if st.DailyReset.IsZero() && st.WeeklyReset.IsZero() {
		t.Error("neither reset time decoded; a hold has no deadline to run to")
	}
	if st.Models == 0 {
		t.Error("no models decoded; field 33 did not read as expected")
	}

	deadline := st.ResetDeadline(time.Now())
	t.Logf("account %s  plan %s  models %d  counters %v", st, st.Plan, st.Models, st.PlanFields)
	if p := st.WeeklyRemainingPercent; p != nil {
		w := "no weekly reset decoded"
		if !st.WeeklyReset.IsZero() {
			w = st.WeeklyReset.Format(time.RFC3339)
		}
		t.Logf("week  %d%% remaining, resets %s", *p, w)
	}
	if p := st.DailyRemainingPercent; p != nil {
		d := "no daily reset decoded"
		if !st.DailyReset.IsZero() {
			d = st.DailyReset.Format(time.RFC3339)
		}
		t.Logf("today %d%% remaining, resets %s", *p, d)
	}
	if deadline.IsZero() {
		t.Logf("no window reads 0%%, so a rate limit on this account would keep the short cooldown")
	} else {
		t.Logf("a quota window reads 0%%, so a rate limit would hold this account out until %s", deadline)
	}
}
