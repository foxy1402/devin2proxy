package devin

import (
	"crypto/rand"
	"math/big"
)

// SessionCodename returns an adjective-noun codename in the shape the CLI uses
// for request field 28 and for its on-disk session names: two lowercase words
// joined by a hyphen, e.g. "warm-soul" or "accurate-cornucopia".
//
// The backend accepts the field being omitted, so this exists only for mimicry —
// and mimicry here means the *shape*, never a fixed literal. A 2026-10-13 capture
// of CLI 3000.11.3 showed the codename to be fresh per conversation (two sessions
// read "warm-soul" and "accurate-cornucopia") and stable across every round of
// one conversation, including the CLI's title side call. Hardcoding an observed
// value would make every proxy request, forever, claim to belong to one named
// session — a fingerprint rather than a disguise.
//
// The word lists are deliberately small and deliberately exclude the codenames
// seen in captures ("warm-soul", "accurate-cornucopia", "jagged-felidae"), so a
// test can assert the generator never emits an observed literal. The codename is
// not a secret and carries no entropy requirement; it only has to look like the
// CLI's.
var codenameAdjectives = []string{
	"cold", "bright", "quiet", "swift", "calm", "bold", "gentle",
	"clever", "steady", "lucky", "brave", "eager", "silent", "golden", "silver",
	"curious", "merry", "noble", "plain", "rapid", "sleek", "vivid",
}

var codenameNouns = []string{
	"river", "falcon", "meadow", "ember", "harbor", "summit", "willow",
	"canyon", "sparrow", "lighthouse", "orchid", "panther", "quartz", "raven",
	"thunder", "voyage", "walnut", "aurora", "basilisk", "cobalt", "dune",
}

// SessionCodename returns a random adjective-noun codename. It never fails: an
// entropy failure falls back to the first entry of each list, because the value
// names a session for the backend's benefit and nothing keys on it here.
func SessionCodename() string {
	adj := codenameAdjectives[randomIndex(len(codenameAdjectives))]
	noun := codenameNouns[randomIndex(len(codenameNouns))]
	return adj + "-" + noun
}

func randomIndex(n int) int {
	if n <= 0 {
		return 0
	}
	i, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(i.Int64())
}
