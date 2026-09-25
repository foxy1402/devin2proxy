package devin

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"devin2proxy/internal/pb"
)

// These tests pin the property that matters most about quota-aware cooldowns: an
// account can only ever be held out because the backend refused it. Quota data may
// lengthen that cooldown, and nothing else — so a status that is unreadable, stale,
// or misread can never take a working account out of rotation.
//
// The same concern runs the other way, which is why the pool-wide refusal guard is
// tested here too: a gateway in front of the backend that starts refusing every
// account must not be allowed to park every account for the rejection period.

// statusAt builds a status whose plan block reports the given daily percentage and
// whose daily reset is at the given time. A percentage of zero is the case the pool
// has to notice: it is the one reading that says a quota window is empty, and it is
// why the percentage is carried as a pointer — absent and zero are different claims.
func statusAt(reset time.Time, dailyPercent int) *AccountStatus {
	return &AccountStatus{
		Email:                 "someone@example.com",
		Plan:                  "Free",
		DailyReset:            reset,
		DailyRemainingPercent: intPtr(dailyPercent),
	}
}

// recordingStatus counts calls and answers with the given status.
func recordingStatus(calls *atomic.Int32, st *AccountStatus, err error) StatusFunc {
	return func(ctx context.Context, creds *Credentials) (*AccountStatus, error) {
		calls.Add(1)
		return st, err
	}
}

// waitFor polls until cond holds, so the tests do not race the background fetch.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// coolingUntilFor reads one slot's cooldown deadline, which is what distinguishes a
// 30-second hold from one that runs to the next reset. The rotation lock is taken
// here rather than the slot's slice read bare, because Remove renumbers the
// cooling slice under that lock. An out-of-range slot reads as zero: never cooled.
func coolingUntilFor(p *Pool, idx int) time.Time {
	p.rot.mu.Lock()
	defer p.rot.mu.Unlock()
	if idx < 0 || idx >= len(p.rot.cooling) {
		return time.Time{}
	}
	return p.rot.cooling[idx]
}

func TestQuotaCooldownHoldsAnExhaustedAccountUntilItsReset(t *testing.T) {
	reset := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	p := poolOf(2)

	// The fetch is held open so the state right after the refusal can be observed
	// without racing it.
	release := make(chan struct{})
	var calls atomic.Int32
	p.SetStatusSource(func(ctx context.Context, creds *Credentials) (*AccountStatus, error) {
		calls.Add(1)
		<-release
		return statusAt(reset, 0), nil
	})

	// The backend refuses first; only then is the account's quota consulted.
	p.Report(p.entries[0], 429, nil)
	if until := coolingUntilFor(p, 0); !until.Before(time.Now().Add(CoolRateLimited + time.Second)) {
		t.Fatalf("the cooldown before any status arrives is %s, want the short %s", until, CoolRateLimited)
	}
	if got := p.QuotaHeld(); got != 0 {
		t.Fatalf("QuotaHeld = %d before the status arrived, want 0", got)
	}

	close(release)
	waitFor(t, "the quota fetch to extend the cooldown", func() bool {
		return coolingUntilFor(p, 0).Equal(reset)
	})
	if got := p.Available(); got != 1 {
		t.Fatalf("Available = %d, want 1: the other account is untouched", got)
	}
	if got := p.QuotaHeld(); got != 1 {
		t.Fatalf("QuotaHeld = %d, want 1", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("status fetched %d times, want 1", got)
	}
}

func TestQuotaCooldownKeepsTheShortHoldWhenQuotaIsNotTheProblem(t *testing.T) {
	// Every counter non-zero means the account still has quota, so the refusal was
	// a plain rate limit and the cooldown must not be extended.
	reset := time.Now().Add(3 * time.Hour)
	p := poolOf(2)
	var calls atomic.Int32
	p.SetStatusSource(recordingStatus(&calls, statusAt(reset, 58), nil))

	p.Report(p.entries[0], 429, nil)
	waitFor(t, "the quota fetch", func() bool { return calls.Load() == 1 })
	time.Sleep(20 * time.Millisecond) // give a wrong extension time to land

	if until := coolingUntilFor(p, 0); until.After(time.Now().Add(2 * CoolRateLimited)) {
		t.Fatalf("cooldown extended to %s although no counter read zero", until)
	}
	if got := p.QuotaHeld(); got != 0 {
		t.Fatalf("QuotaHeld = %d, want 0", got)
	}
}

func TestQuotaCooldownFailsOpenWhenStatusIsUnreadable(t *testing.T) {
	// The single most important failure mode: if the status cannot be read, the
	// account keeps exactly the cooldown the backend earned. No status, no
	// extension, no error surfaced.
	p := poolOf(2)
	var calls atomic.Int32
	p.SetStatusSource(recordingStatus(&calls, nil, errors.New("status endpoint unreachable")))

	p.Report(p.entries[0], 429, nil)
	waitFor(t, "the failed status fetch", func() bool { return calls.Load() == 1 })
	time.Sleep(20 * time.Millisecond)

	if until := coolingUntilFor(p, 0); until.After(time.Now().Add(2 * CoolRateLimited)) {
		t.Fatalf("a failed status fetch extended the cooldown to %s", until)
	}
	if got := p.QuotaHeld(); got != 0 {
		t.Fatalf("QuotaHeld = %d, want 0", got)
	}
}

func TestQuotaStatusAloneNeverCoolsAnAccount(t *testing.T) {
	// Reading an exhausted status is not by itself a reason to bench anything: the
	// account has to have been refused first. This is what stops a pre-flight check,
	// or a stale snapshot, from taking a working account out of rotation.
	p := poolOf(2)
	reset := time.Now().Add(6 * time.Hour)
	p.SetStatusSource(recordingStatus(new(atomic.Int32), statusAt(reset, 0), nil))

	// No Report has happened, so nothing should be cooling.
	if got := p.Available(); got != 2 {
		t.Fatalf("Available = %d, want 2 with no refusal recorded", got)
	}
	// And applying an exhausted status to a slot that is not cooling is a no-op.
	p.applyQuota(p.entries[0], statusAt(reset, 0))
	if got := p.Available(); got != 2 {
		t.Fatalf("Available = %d, want 2: quota evidence must not create a cooldown", got)
	}
	if got := p.QuotaHeld(); got != 0 {
		t.Fatalf("QuotaHeld = %d, want 0", got)
	}
}

func TestQuotaCooldownNeverShortensAndIsCapped(t *testing.T) {
	t.Run("a reset sooner than the current deadline is ignored", func(t *testing.T) {
		p := poolOf(1)
		// A long cooldown already in place, from a rejection.
		p.Report(p.entries[0], 401, nil)
		before := coolingUntilFor(p, 0)

		p.applyQuota(p.entries[0], statusAt(time.Now().Add(time.Minute), 0))
		if after := coolingUntilFor(p, 0); !after.Equal(before) {
			t.Fatalf("cooldown moved from %s to %s; quota evidence must only ever extend", before, after)
		}
	})

	t.Run("a refusal cannot shorten a quota hold", func(t *testing.T) {
		// The two kinds of evidence say different things, and the longer one wins:
		// a gateway that answers 401 for an account which is genuinely out of quota
		// must not drop a multi-hour hold back to minutes.
		p := poolOf(1)
		reset := time.Now().Add(3 * time.Hour)
		p.Report(p.entries[0], 429, nil)
		p.applyQuota(p.entries[0], statusAt(reset, 0))
		if got := coolingUntilFor(p, 0); !got.Equal(reset) {
			t.Fatalf("setup: cooldown = %s, want the reset at %s", got, reset)
		}

		p.Report(p.entries[0], 401, nil)
		until := coolingUntilFor(p, 0)
		if until.Before(reset.Add(-time.Second)) {
			t.Fatalf("a rejection pulled the quota hold from %s back to %s", reset, until)
		}
	})

	t.Run("a reset beyond the cap is clamped", func(t *testing.T) {
		p := poolOf(1)
		p.Report(p.entries[0], 429, nil)
		p.applyQuota(p.entries[0], statusAt(time.Now().Add(100*time.Hour), 0))

		until := coolingUntilFor(p, 0)
		if until.After(time.Now().Add(MaxQuotaCool + time.Minute)) {
			t.Fatalf("cooldown ran to %s, beyond the %s cap", until, MaxQuotaCool)
		}
		if until.Before(time.Now().Add(MaxQuotaCool - time.Minute)) {
			t.Fatalf("cooldown %s is shorter than the cap it should have been clamped to", until)
		}
	})

	t.Run("a slot that has recovered is left alone", func(t *testing.T) {
		p := poolOf(1)
		p.Report(p.entries[0], 429, nil)
		// The cooldown lapses, so the account is serving again.
		p.rot.mu.Lock()
		p.rot.cooling[0] = time.Now().Add(-time.Second)
		p.rot.mu.Unlock()

		p.applyQuota(p.entries[0], statusAt(time.Now().Add(6*time.Hour), 0))
		if got := p.Available(); got != 1 {
			t.Fatalf("Available = %d, want 1: late evidence must not bench an account that recovered", got)
		}
	})
}

func TestQuotaHeldStopsCountingAfterTheResetPasses(t *testing.T) {
	// /healthz reports the count, so a hold that has expired has to stop being
	// counted; otherwise the number only ever grows and says nothing.
	p := poolOf(1)
	p.Report(p.entries[0], 429, nil)
	p.applyQuota(p.entries[0], statusAt(time.Now().Add(time.Minute), 0))
	if got := p.QuotaHeld(); got != 1 {
		t.Fatalf("QuotaHeld = %d, want 1 while the hold is in force", got)
	}

	// Move both deadlines into the past, which is what waiting would do.
	p.rot.mu.Lock()
	p.rot.cooling[0] = time.Now().Add(-time.Second)
	p.rot.mu.Unlock()
	p.quotaMu.Lock()
	p.quotaCoolUntil[0] = time.Now().Add(-time.Second)
	p.quotaMu.Unlock()

	if got := p.QuotaHeld(); got != 0 {
		t.Fatalf("QuotaHeld = %d, want 0 once the hold has expired", got)
	}
	if got := p.Available(); got != 1 {
		t.Fatalf("Available = %d, want 1: the account is back in rotation", got)
	}
}

func TestQuotaStatusIsOnlyFetchedAfterARefusal(t *testing.T) {
	// The status call itself costs a request against the account, so it is only
	// worth making when the answer could change something: a refusal that the quota
	// might explain. Everything else — a cancel, a transport failure, a backend
	// fault, a malformed request, an expired token — asks nothing.
	cases := []struct {
		name   string
		status int
		err    error
		want   int32
	}{
		{"a cancellation asks nothing", 0, context.Canceled, 0},
		{"a cancellation wrapped in a route error asks nothing", 0,
			&EgressError{Op: "dial", Err: context.Canceled}, 0},
		{"a transport failure asks nothing", 0, errors.New("dial tcp: connection refused"), 0},
		{"a route failure asks nothing", 0,
			&EgressError{Op: "dial", Err: errors.New("socks5: connection refused"), Broken: true}, 0},
		{"a backend fault asks nothing", 0, &HTTPError{Status: 503, Body: []byte("down")}, 0},
		{"a backend fault in connect form asks nothing", 0, &ConnectError{Code: "unavailable"}, 0},
		{"a malformed request asks nothing", 0, &ConnectError{Code: "invalid_argument"}, 0},
		{"a failed precondition asks nothing", 0, &ConnectError{Code: "failed_precondition"}, 0},
		{"a backend timeout asks nothing", 0, &ConnectError{Code: "deadline_exceeded"}, 0},
		{"a rejected credential asks nothing", 0, &ConnectError{Code: "unauthenticated"}, 0},
		{"an http rejection asks nothing", 0, &HTTPError{Status: 403}, 0},
		{"a rate limit asks once", 0, &HTTPError{Status: 429}, 1},
		{"a rate limit in connect form asks once", 0, &ConnectError{Code: "resource_exhausted"}, 1},
		{"a rate limit relayed by a route asks once", 0,
			&EgressError{Op: "roundtrip", Err: &ConnectError{Code: "resource_exhausted"}}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := poolOf(2)
			var calls atomic.Int32
			p.SetStatusSource(recordingStatus(&calls, statusAt(time.Now().Add(time.Hour), 0), nil))

			p.Report(p.entries[0], tc.status, tc.err)
			time.Sleep(30 * time.Millisecond)
			if got := calls.Load(); got != tc.want {
				t.Fatalf("status fetched %d times, want %d", got, tc.want)
			}
		})
	}
}

// TestQuotaBookkeepingSurvivesConcurrentRemovals is the stress test behind the
// lock discipline in refreshQuota, Remember and applyQuota: all of them resolve
// a slot and act on it, and a Removal running between the two used to be able
// to renumber the per-slot slices under them — an out-of-range index in an
// unrecovered goroutine, or quota state landing on the wrong account. Any
// regression back to carrying an index across a lock handoff shows up here as a
// panic, given enough iterations.
func TestQuotaBookkeepingSurvivesConcurrentRemovals(t *testing.T) {
	p := poolOf(4)
	var calls atomic.Int32
	p.SetStatusSource(func(ctx context.Context, creds *Credentials) (*AccountStatus, error) {
		calls.Add(1)
		// A real fetch takes long enough for the list to change under it.
		time.Sleep(time.Millisecond)
		return statusAt(time.Now().Add(50*time.Millisecond), 0), nil
	})

	done := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				// A 429 is what sends refreshQuota off to the backend while the
				// membership churns; Remember and applyQuota land later, on the
				// answer.
				creds := p.Next()
				p.Report(creds, 429, nil)
				p.Remember(creds, &AccountStatus{Email: "someone@example.com"})
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			p.Remove(i % max(1, p.Len()))
			p.Add(tokenAt(100 + i))
			p.RemoveCredential(p.CredentialAt(0))
			p.Add(tokenAt(300 + i))
		}
		close(done)
	}()

	select {
	case <-done:
		wg.Wait()
	case <-time.After(30 * time.Second):
		t.Fatal("the churn deadlocked; a lock order inverted somewhere")
	}
	// Whatever the churn, the per-slot slices must still agree with the entries
	// they index: that len invariant is what applyQuota and ClearCooldown lean on.
	if len(p.busy) != p.Len() || len(p.quotaCoolUntil) != p.Len() || len(p.rot.cooling) != p.Len() {
		t.Fatalf("slices diverged: %d busy, %d quota, %d cooling, %d entries",
			len(p.busy), len(p.quotaCoolUntil), len(p.rot.cooling), p.Len())
	}
}

func TestQuotaStatusIsNotFetchedPerRequestDuringARefusalStorm(t *testing.T) {
	// An IDE retrying into a rate limit must not turn into one status call per
	// request.
	p := poolOf(2)
	var calls atomic.Int32
	release := make(chan struct{})
	p.SetStatusSource(func(ctx context.Context, creds *Credentials) (*AccountStatus, error) {
		calls.Add(1)
		<-release
		return statusAt(time.Now().Add(time.Hour), 0), nil
	})

	for i := 0; i < 8; i++ {
		p.Report(p.entries[0], 429, nil)
	}
	close(release)
	waitFor(t, "the single status fetch to finish", func() bool { return calls.Load() >= 1 })
	time.Sleep(30 * time.Millisecond)

	if got := calls.Load(); got != 1 {
		t.Fatalf("status fetched %d times during a burst of 8 refusals, want 1", got)
	}
}

func TestQuotaStatusIsNotFetchedAgainWhileTheHoldStands(t *testing.T) {
	// Once a hold is in place the answer cannot change until the reset it named, so
	// a request arriving while the pool is exhausted must not turn into another
	// status call. Without this, the state where every account is out of quota is
	// also the state that asks the backend the most questions.
	p := poolOf(2)
	var calls atomic.Int32
	p.SetStatusSource(recordingStatus(&calls, statusAt(time.Now().Add(3*time.Hour), 0), nil))

	p.Report(p.entries[0], 429, nil)
	waitFor(t, "the first status fetch", func() bool { return calls.Load() == 1 })
	waitFor(t, "the hold to be in place", func() bool { return p.QuotaHeld() == 1 })

	// Four more refusals on the same account, as a retrying client would produce.
	for i := 0; i < 4; i++ {
		p.Report(p.entries[0], 429, nil)
	}
	time.Sleep(30 * time.Millisecond)

	if got := calls.Load(); got != 1 {
		t.Fatalf("status fetched %d times, want 1 while the hold stands", got)
	}
}

func TestQuotaCooldownDisabledByDefault(t *testing.T) {
	// Without a status source the pool behaves exactly as it did before the feature
	// existed: a rate limit is a 30-second hold and no network call is made on its
	// behalf.
	p := poolOf(2)
	p.Report(p.entries[0], 429, nil)
	if until := coolingUntilFor(p, 0); until.After(time.Now().Add(2 * CoolRateLimited)) {
		t.Fatalf("cooldown was extended with no status source configured: %s", until)
	}
	if got := p.QuotaHeld(); got != 0 {
		t.Fatalf("QuotaHeld = %d, want 0", got)
	}
}

func TestPoolWideRefusalDoesNotParkEveryAccount(t *testing.T) {
	t.Run("the pool is pulled back together", func(t *testing.T) {
		// Three accounts rejected in the same window is the signature of a
		// provider-side change rather than three bad keys. The guard fires on the
		// refusal that empties the pool, and it has to reach back to the two that
		// were parked moments before it: otherwise one gateway incident parks the
		// whole pool, one account per request, for the rejection period.
		p := poolOf(3)
		for _, creds := range p.entries {
			p.Report(creds, 401, nil)
		}

		for i := range p.entries {
			if until := coolingUntilFor(p, i); until.After(time.Now().Add(2 * CoolRateLimited)) {
				t.Fatalf("account %d held until %s; a pool-wide refusal should not park an account "+
					"for the rejection period", i, until)
			}
		}
		if got := p.Available(); got != 0 {
			t.Fatalf("Available = %d, want 0: every account is still held, just briefly", got)
		}
	})

	t.Run("a single-account pool keeps the rejection cooldown", func(t *testing.T) {
		// The guard is about *other* accounts still being usable. With one account
		// there is nobody else to protect, and the pool serves the soonest recovery
		// regardless, so the normal cooldown stands.
		p := poolOf(1)
		p.Report(p.entries[0], 401, nil)
		if until := coolingUntilFor(p, 0); !until.After(time.Now().Add(2 * CoolRateLimited)) {
			t.Fatalf("cooldown = %s, want the rejection period for a single-account pool", until)
		}
	})

	t.Run("a quota hold is not pulled back by it", func(t *testing.T) {
		// A hold that a status call confirmed is not a symptom of the refusals
		// around it, so the retraction leaves it alone. Account 0 is out of quota
		// until its reset; account 1 is refused and empties the pool.
		p := poolOf(2)
		reset := time.Now().Add(3 * time.Hour)
		p.Report(p.entries[0], 429, nil)
		p.applyQuota(p.entries[0], statusAt(reset, 0))

		p.Report(p.entries[1], 401, nil)

		if got := coolingUntilFor(p, 0); !got.Equal(reset) {
			t.Fatalf("the quota hold was retracted to %s, want the reset at %s", got, reset)
		}
		if got := coolingUntilFor(p, 1); got.After(time.Now().Add(2 * CoolRateLimited)) {
			t.Fatalf("account 1 held until %s, want the short cooldown", got)
		}
		if got := p.QuotaHeld(); got != 1 {
			t.Fatalf("QuotaHeld = %d, want 1", got)
		}
	})
}

func TestAccountStatusResetDeadline(t *testing.T) {
	now := time.Now()
	daily := now.Add(2 * time.Hour)
	weekly := now.Add(5 * 24 * time.Hour)

	cases := []struct {
		name   string
		status *AccountStatus
		want   time.Time
	}{
		{"a window at 0% waits for its reset", statusAt(daily, 0), daily},
		{"a window with quota left means no deadline", statusAt(daily, 58), time.Time{}},
		{"a zero on a window that has no reset yet is still no deadline",
			&AccountStatus{DailyRemainingPercent: intPtr(0)}, time.Time{}},
		{"the weekly reset is used when the daily one has passed", &AccountStatus{
			DailyRemainingPercent: intPtr(0),
			DailyReset:            now.Add(-time.Hour),
			WeeklyReset:           weekly,
		}, weekly},
		{"the sooner of two empty windows wins", &AccountStatus{
			DailyRemainingPercent:  intPtr(0),
			WeeklyRemainingPercent: intPtr(0),
			DailyReset:             daily,
			WeeklyReset:            weekly,
		}, daily},
		{"an empty weekly window waits for the weekly reset", &AccountStatus{
			WeeklyRemainingPercent: intPtr(0),
			DailyReset:             daily,
			WeeklyReset:            weekly,
		}, weekly},
		// The unnamed counters keep the older, blunter reading, but only when no
		// percentage came back at all: a present 58% is evidence the account is fine,
		// and a zero on a field that may be a status enum must not outrank it.
		{"a zero on an unnamed counter counts when no percentage came back", &AccountStatus{
			PlanFields: map[int]int64{8: 0, 9: 500},
			DailyReset: daily,
		}, daily},
		{"a present, non-zero percentage outranks an unnamed counter at zero", &AccountStatus{
			DailyRemainingPercent:  intPtr(58),
			WeeklyRemainingPercent: intPtr(41),
			PlanFields:             map[int]int64{8: 0},
			DailyReset:             daily,
		}, time.Time{}},
		{"no resets at all means no deadline", &AccountStatus{DailyRemainingPercent: intPtr(0)}, time.Time{}},
		{"no plan block at all means no deadline", &AccountStatus{DailyReset: daily}, time.Time{}},
		{"a nil status is harmless", nil, time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.status.ResetDeadline(now); !got.Equal(tc.want) {
				t.Fatalf("ResetDeadline = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestAnAbsentPercentageIsNotAQuotaOfZero pins the distinction the page depends on.
// The wire format cannot say "0%" for a proto3 field whose value is the default, and
// the backend simply leaves the percentage out when it has nothing to report — so a
// decoder that stores a plain int would show every unreported window as empty. The
// fields are pointers for this reason: nil means "not reported", a non-nil 0 means
// the window is out of quota, and only the second one is worth colouring red.
func TestAnAbsentPercentageIsNotAQuotaOfZero(t *testing.T) {
	planWith := func(percent *int, weekly bool) []byte {
		w := pb.NewWriter()
		w.Message(13, func(plan *pb.Writer) {
			plan.Message(1, func(details *pb.Writer) { details.String(2, "Free") })
			if percent != nil {
				field := 14
				if weekly {
					field = 15
				}
				plan.Varint(field, uint64(*percent))
			}
		})
		return w.Bytes()
	}

	if got := DecodeAccountStatus(planWith(nil, false)); got.DailyRemainingPercent != nil {
		t.Fatalf("an absent percentage decoded as %d, want absent", *got.DailyRemainingPercent)
	}

	zero := 0
	decoded := DecodeAccountStatus(planWith(&zero, false))
	if decoded.DailyRemainingPercent == nil {
		t.Fatal("a present 0% decoded as absent: the page would read that as 'not reported'")
	}
	if *decoded.DailyRemainingPercent != 0 {
		t.Fatalf("a present 0%% decoded as %d", *decoded.DailyRemainingPercent)
	}
	if decoded.WeeklyRemainingPercent != nil {
		t.Fatalf("the weekly window decoded as %d though only the daily one was sent",
			*decoded.WeeklyRemainingPercent)
	}
}

func TestDecodeAccountStatusReadsBothShapes(t *testing.T) {
	// The live response wraps the account at field 1 and the CLI's cache stores the
	// inner message alone; both must decode to the same thing.
	inner := encodeTestStatus("Test Name", "someone@example.com", "user-1", "devin-team$a", 2)
	w := pb.NewWriter()
	w.RawBytes(1, inner)
	wrapped := w.Bytes()

	for name, body := range map[string][]byte{"wrapped": wrapped, "bare": inner} {
		t.Run(name, func(t *testing.T) {
			got := DecodeAccountStatus(body)
			if got.Name != "Test Name" || got.Email != "someone@example.com" || got.UserID != "user-1" {
				t.Fatalf("decoded %+v", got)
			}
			if got.TeamID != "devin-team$a" || got.TeamStatus != 2 {
				t.Fatalf("team fields decoded as %q/%d", got.TeamID, got.TeamStatus)
			}
			if got.Plan != "Free" {
				t.Fatalf("plan = %q, want Free", got.Plan)
			}
			if got.Models != 1 {
				t.Fatalf("models = %d, want 1", got.Models)
			}
			if !got.DailyReset.Equal(time.Unix(1790236800, 0).UTC()) {
				t.Fatalf("daily reset = %s", got.DailyReset)
			}
			if !got.WeeklyReset.Equal(time.Unix(1790496000, 0).UTC()) {
				t.Fatalf("weekly reset = %s", got.WeeklyReset)
			}
			// The two percentages are the numbers the CLI prints on its main page, so
			// they have to arrive as their own fields rather than as raw counters.
			if got.DailyRemainingPercent == nil || *got.DailyRemainingPercent != 58 {
				t.Fatalf("daily percentage = %v, want 58", got.DailyRemainingPercent)
			}
			if got.WeeklyRemainingPercent == nil || *got.WeeklyRemainingPercent != 41 {
				t.Fatalf("weekly percentage = %v, want 41", got.WeeklyRemainingPercent)
			}
			if got.OverageBalanceMicros == nil || *got.OverageBalanceMicros != -36850 {
				t.Fatalf("overage balance = %v, want -36850: the field is signed", got.OverageBalanceMicros)
			}
			if got.PlanFields[8] != 2500 {
				t.Fatalf("plan field 8 = %d, want 2500", got.PlanFields[8])
			}
			// The named fields must not also land in the catch-all, or the page would
			// draw every number twice and the pool would read the percentages as
			// anonymous counters again.
			for _, named := range []int{14, 15, 16, 17, 18} {
				if v, ok := got.PlanFields[named]; ok {
					t.Fatalf("plan field %d = %d is named now and must not be a raw counter", named, v)
				}
			}
			if len(got.ModelList) != 1 {
				t.Fatalf("model list = %+v", got.ModelList)
			}
			m := got.ModelList[0]
			if m.Name != "a model" || m.UID != "MODEL_A" || m.Provider != "OpenAI" {
				t.Fatalf("model = %+v", m)
			}
			if m.Context != 128000 || m.MaxOutput != 16384 {
				t.Fatalf("limits = %d/%d, want 128000/16384", m.Context, m.MaxOutput)
			}
			if len(m.Rates) != 1 || m.Rates[0].Label != "Input" || m.Rates[0].Unit != "1M tokens" {
				t.Fatalf("rates = %+v", m.Rates)
			}
		})
	}
}

func TestDecodeAccountStatusModelList(t *testing.T) {
	t.Run("a uid is taken from the spec block when the entry has none", func(t *testing.T) {
		w := pb.NewWriter()
		w.Message(33, func(models *pb.Writer) {
			models.Message(1, func(m *pb.Writer) {
				m.String(1, "a model")
				m.Message(23, func(spec *pb.Writer) { spec.String(17, "MODEL_FROM_SPEC") })
			})
		})
		got := DecodeAccountStatus(w.Bytes())
		if len(got.ModelList) != 1 || got.ModelList[0].UID != "MODEL_FROM_SPEC" {
			t.Fatalf("model list = %+v", got.ModelList)
		}
	})

	t.Run("only the provider group names a provider", func(t *testing.T) {
		// The sibling group is "Cost", which lists the same model names and carries no
		// provider at all. Only the group whose heading says "Provider" may be read
		// as one, or every model would come back with an empty provider set from it.
		w := pb.NewWriter()
		w.Message(33, func(models *pb.Writer) {
			models.Message(1, func(m *pb.Writer) { m.String(1, "a model") })
			models.Message(2, func(group *pb.Writer) {
				group.String(1, "Cost")
				group.Message(2, func(g *pb.Writer) { g.String(2, "a model") })
			})
		})
		got := DecodeAccountStatus(w.Bytes())
		if got.ModelList[0].Provider != "" {
			t.Fatalf("provider = %q, want empty: the Cost group is not a provider", got.ModelList[0].Provider)
		}
	})

	t.Run("no model list at all is not an error", func(t *testing.T) {
		// This is the shape the CLI's own cache holds, and the pool must treat it as
		// "nothing known about the models" rather than as an unreadable status.
		w := pb.NewWriter()
		w.String(7, "someone@example.com")
		got := DecodeAccountStatus(w.Bytes())
		if got.Models != 0 || got.ModelList != nil {
			t.Fatalf("models = %d, list = %+v", got.Models, got.ModelList)
		}
	})

	t.Run("a rate row's note is carried", func(t *testing.T) {
		w := pb.NewWriter()
		w.Message(33, func(models *pb.Writer) {
			models.Message(1, func(m *pb.Writer) {
				m.String(1, "xAI Grok-3 mini Thinking")
				m.Message(32, func(rate *pb.Writer) {
					rate.String(1, "Input")
					rate.String(3, "1M tokens")
					rate.String(7, "Higher effort consumes more tokens")
				})
			})
		})
		got := DecodeAccountStatus(w.Bytes())
		rate := got.ModelList[0].Rates[0]
		if rate.Note != "Higher effort consumes more tokens" {
			t.Fatalf("note = %q", rate.Note)
		}
	})
}

// encodeTestStatus builds the fields of a UserStatus message the tests need: the
// identity strings, the plan block with its nested name and counters, and a model
// list holding one model with its limits, its rate rows and a provider group.
func encodeTestStatus(name, email, userID, teamID string, teamStatus uint64) []byte {
	w := pb.NewWriter()
	w.String(3, name)
	w.String(5, teamID)
	w.Varint(6, teamStatus)
	w.String(7, email)
	w.Message(13, func(plan *pb.Writer) {
		plan.Message(1, func(details *pb.Writer) { details.String(2, "Free") })
		plan.Varint(8, 2500)
		// The live numbers for a free account mid-week, as the CLI's own main page
		// prints them: 41% of the weekly window left, 3d 7h before it refills.
		plan.Varint(14, 58)
		plan.Varint(15, 41)
		// Field 16 is signed: a negative value is a ten-byte varint, which is the
		// shape a misread of the field width would turn into a huge positive number.
		plan.Int64(16, -36850)
		plan.Varint(17, 1790236800)
		plan.Varint(18, 1790496000)
	})
	w.Message(33, encodeTestModels)
	w.String(36, userID)
	return w.Bytes()
}

// encodeTestModels writes a model list in the live shape: one model with a uid,
// a spec block and rate rows, plus the provider grouping that says which provider
// it belongs to.
func encodeTestModels(models *pb.Writer) {
	models.Message(1, func(m *pb.Writer) {
		m.String(1, "a model")
		m.String(22, "MODEL_A")
		m.Message(23, func(spec *pb.Writer) {
			spec.Varint(4, 128000)
			spec.Varint(13, 16384)
			spec.String(17, "MODEL_A")
		})
		m.Message(32, func(rate *pb.Writer) {
			rate.String(1, "Input")
			rate.String(3, "1M tokens")
		})
	})
	models.Message(2, func(group *pb.Writer) {
		group.String(1, "Provider")
		group.Message(2, func(p *pb.Writer) {
			p.String(1, "OpenAI")
			p.String(2, "a model")
		})
	})
}

func TestQuotaCooldownExtensionIsVisiblyLogged(t *testing.T) {
	// A hold that lasts hours is surprising enough that it has to appear in the log,
	// with the account named and the reset time spelled out.
	var sink syncBuffer
	log.SetOutput(&sink)
	defer log.SetOutput(os.Stderr)

	reset := time.Now().Add(4 * time.Hour).Truncate(time.Second)
	p := poolOf(2)
	p.SetStatusSource(recordingStatus(new(atomic.Int32), statusAt(reset, 0), nil))
	p.Report(p.entries[0], 429, nil)

	waitFor(t, "the log line", func() bool {
		return strings.Contains(sink.String(), "no quota left")
	})
	if got := sink.String(); !strings.Contains(got, reset.UTC().Format(time.RFC3339)) {
		t.Fatalf("the log line does not name the reset time:\n%s", got)
	}
	if got := sink.String(); !strings.Contains(got, "someone@example.com") {
		t.Fatalf("the log line does not name the account:\n%s", got)
	}
}

// syncBuffer is a log sink a background goroutine can write to while the test
// reads it, which a plain bytes.Buffer does not allow.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
