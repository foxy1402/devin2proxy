package devin

import (
	"sync"
	"time"
)

// rotationCursor is the shared half of the two pools that rotate per request:
// the account pool and the outbound-route pool. Both walk a fixed-size
// collection, skip whatever is cooling down, and fall back to the
// soonest-recovering entry when everything is cooling. Only what counts as a
// failure, and how long its cooldown lasts, differ between them.
//
// Rotation is per request by design. Nothing here is conversation-affine: each
// request opens a fresh backend trajectory and resends its own history, so which
// entry serves it cannot matter to the result.
type rotationCursor struct {
	mu      sync.Mutex
	next    int
	cooling []time.Time
}

func newRotationCursor(n int) rotationCursor {
	return rotationCursor{cooling: make([]time.Time, n)}
}

// Len reports how many slots the cursor has. The count changes when the pool
// gains or loses an account, both of which keep the walk position meaningful.
func (c *rotationCursor) Len() int { return len(c.cooling) }

// appendSlot grows the cursor by one warm slot, so the pool can take on another
// account without losing the cooldowns of the ones it already has. Call it with
// the lock held.
func (c *rotationCursor) appendSlot() { c.cooling = append(c.cooling, time.Time{}) }

// removeSlot drops one slot and keeps the walk position meaningful: `next` is an
// index into the remaining slots, so removing something before it has to move it
// back by one, and a position left at or past the shrunken end has to wrap.
// Call it with the lock held.
func (c *rotationCursor) removeSlot(i int) {
	if i < 0 || i >= len(c.cooling) {
		return
	}
	c.cooling = append(c.cooling[:i], c.cooling[i+1:]...)
	if i < c.next {
		c.next--
	}
	// Removing the slot `next` itself pointed at leaves the position unchanged
	// while the list has one entry fewer — and when that was the last entry,
	// `next` now equals len(cooling). pick() mods `next` while walking, but its
	// all-cooling fallback indexes cooling[next] directly, which is where a
	// stranded position panicked the next request.
	if c.next >= len(c.cooling) {
		c.next = 0
	}
}

// pick returns the index to use for the next request, or -1 when there are no
// slots. Call it with the lock held.
//
// Entries that are cooling are skipped. If every entry is cooling the
// soonest-recovering one is returned anyway: serving a possibly-broken entry is
// better than failing outright, since the caller reports the outcome back here
// and the entry may already have recovered.
func (c *rotationCursor) pick(now time.Time) int {
	n := len(c.cooling)
	if n == 0 {
		return -1
	}
	// Belt against any future mutation that shrinks the list without adjusting
	// the cursor: the all-cooling fallback below indexes cooling[next] directly.
	if c.next >= n {
		c.next = 0
	}
	for i := 0; i < n; i++ {
		idx := (c.next + i) % n
		if now.Before(c.cooling[idx]) {
			continue
		}
		c.next = (idx + 1) % n
		return idx
	}

	best := c.next
	for i := 1; i < n; i++ {
		idx := (c.next + i) % n
		if c.cooling[idx].Before(c.cooling[best]) {
			best = idx
		}
	}
	c.next = (best + 1) % n
	return best
}

// availableLocked counts the slots that are not cooling. Call it with the lock
// held.
func (c *rotationCursor) availableLocked(now time.Time) int {
	n := 0
	for _, t := range c.cooling {
		if !now.Before(t) {
			n++
		}
	}
	return n
}

// coolOff takes a slot out of rotation until the given time, and reports whether
// it was already cooling. An already-cooling slot means this is a duplicate
// report — several concurrent requests can fail together — rather than a new
// episode, which is what decides whether it is worth a log line.
//
// Warm or cool, the deadline is pushed out: an entry that is still failing has
// not earned its way back in. It is never pulled *in*, though. A slot already
// held out for longer than this refusal would hold it keeps its longer deadline,
// because the two pieces of evidence are about different things — how much the
// account has consumed, versus how one request went — and only the longer one is
// safe to act on. Call it with the lock held.
func (c *rotationCursor) coolOff(idx int, now, until time.Time) (alreadyCooling bool) {
	alreadyCooling = now.Before(c.cooling[idx])
	if until.After(c.cooling[idx]) {
		c.cooling[idx] = until
	}
	return alreadyCooling
}

// clampCooling pulls a slot's deadline back to at most until, and reports whether
// it moved. It is the only way a cooldown is ever shortened, and it exists for
// exactly one caller: the pool-wide-refusal guard, which needs to retract the
// rejection cooldowns that a systemic failure has just been shown to be the cause
// of. Call it with the lock held.
func (c *rotationCursor) clampCooling(idx int, now, until time.Time) bool {
	if idx < 0 || idx >= len(c.cooling) {
		return false
	}
	if !until.Before(c.cooling[idx]) {
		return false
	}
	c.cooling[idx] = until
	return true
}

// extendCooling pushes an already-cooling slot's deadline out to until, and
// reports whether it did.
//
// Two cases deliberately do nothing: a slot whose cooldown has already expired,
// because it has been serving requests again and late evidence must not bench it;
// and a deadline that would move the slot *earlier*, because this is only ever
// used to add to a cooldown, never to shorten one. Call it with the lock held.
func (c *rotationCursor) extendCooling(idx int, now, until time.Time) bool {
	if idx < 0 || idx >= len(c.cooling) {
		return false
	}
	if !now.Before(c.cooling[idx]) {
		return false
	}
	if !until.After(c.cooling[idx]) {
		return false
	}
	c.cooling[idx] = until
	return true
}

// coolingUntil reports the deadline a slot is cooling until, and whether it is
// cooling at all. Call it with the lock held.
func (c *rotationCursor) coolingUntil(idx int, now time.Time) (time.Time, bool) {
	if idx < 0 || idx >= len(c.cooling) {
		return time.Time{}, false
	}
	return c.cooling[idx], now.Before(c.cooling[idx])
}
