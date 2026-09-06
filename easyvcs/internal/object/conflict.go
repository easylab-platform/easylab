package object

import (
	"encoding/hex"
	"fmt"
	"strings"
)

// Conflict is an N-way merge conflict, expressed as a set of negative terms
// (the base versions, "removes") and positive terms (the side versions,
// "adds"). This mirrors jj's `Merge<T>` where a 3-way conflict is represented
// as [-base, +ours, +theirs]; a conflict can generalize to any number of
// terms. Each term references a blob id plus a human label describing where it
// came from.
type Conflict struct {
	Removes []Term
	Adds    []Term
}

// Term is one side of a conflict: a blob id plus a label.
type Term struct {
	ID    ID
	Label string
}

// NewConflictFrom3Way builds a conflict from a classic 3-way merge:
// base is negative; ours and theirs are positive.
func NewConflictFrom3Way(base, ours, theirs ID) *Conflict {
	return &Conflict{
		Removes: []Term{{ID: base, Label: "base"}},
		Adds: []Term{
			{ID: ours, Label: "ours"},
			{ID: theirs, Label: "theirs"},
		},
	}
}

// Numeric returns the number of terms (removes + adds).
func (c *Conflict) Numeric() int { return len(c.Removes) + len(c.Adds) }

// pickTerm selects the add term at the given index (0-based among Adds).
func (c *Conflict) pickTerm(idx int) (Term, bool) {
	if idx < 0 || idx >= len(c.Adds) {
		return Term{}, false
	}
	return c.Adds[idx], true
}

// resolveToAdd returns a conflict reduced to a single side by index.
func resolveToAdd(c *Conflict, idx int) (ID, bool) {
	t, ok := c.pickTerm(idx)
	return t.ID, ok
}

// conflictMagic is the canonical serialization header for conflict objects.
const conflictMagic = "easyvcs-conflict-v1"

// encodeConflictBytes produces the canonical byte form of a conflict.
func encodeConflictBytes(c *Conflict) []byte {
	var b strings.Builder
	b.WriteString(conflictMagic)
	b.WriteByte('\n')
	fmt.Fprintf(&b, "%d\n", len(c.Removes))
	for _, t := range c.Removes {
		fmt.Fprintf(&b, "%s\n%s\n", hex.EncodeToString(t.ID[:]), t.Label)
	}
	fmt.Fprintf(&b, "%d\n", len(c.Adds))
	for _, t := range c.Adds {
		fmt.Fprintf(&b, "%s\n%s\n", hex.EncodeToString(t.ID[:]), t.Label)
	}
	return []byte(b.String())
}

// decodeConflictBytes parses the canonical byte form back into a Conflict.
func decodeConflictBytes(payload []byte) *Conflict {
	rest := strings.TrimSuffix(string(payload), "\n")
	lines := strings.Split(rest, "\n")
	if len(lines) < 2 {
		return &Conflict{}
	}
	idx := 1
	readTerms := func() []Term {
		n := 0
		if idx < len(lines) {
			fmt.Sscanf(lines[idx], "%d", &n)
			idx++
		}
		terms := make([]Term, 0, n)
		for i := 0; i < n && idx+1 < len(lines); i++ {
			var id ID
			if b, err := hex.DecodeString(lines[idx]); err == nil && len(b) == len(id) {
				copy(id[:], b)
			}
			label := lines[idx+1]
			idx += 2
			terms = append(terms, Term{ID: id, Label: label})
		}
		return terms
	}
	return &Conflict{Removes: readTerms(), Adds: readTerms()}
}

// IsConflictPayload reports whether a payload looks like an encoded conflict.
func IsConflictPayload(payload []byte) bool {
	p := string(payload)
	return strings.HasPrefix(p, conflictMagic)
}

// IsConflictObject reports whether an object kind is a conflict.
func IsConflictObject(k Kind) bool { return k == KindConflict }
