// Package stepidentity computes the approval hash agreed with the Synaptum and Axonium teams (A-43,
// INT-011): a SHA-256 over the RFC 8785 (JCS) canonicalization of a step's identity.
//
// What the hash is FOR: an approval decision is taken by a person, minutes or hours after the loop
// asked for it, and it must bind to THAT step of THAT run with THOSE arguments. Re-deriving the hash
// at execution time and comparing is what makes a mutated argument fail closed instead of running
// under an approval granted for something else.
//
// Why JCS and not encoding/json: the two sides of this seam are a Go gateway and a Python loop, and
// they disagree on JSON in three independent ways — Go HTML-escapes <, > and &; Go writes 1 for a
// float64 1.0 where Python writes 1.0; Python escapes non-ASCII by default. Two of those are flags.
// NUMBERS ARE NOT A FLAG IN EITHER LANGUAGE, which is why a shared canonicalization had to be adopted
// rather than configured.
//
// The canonicalizer here is github.com/gowebpki/jcs, the WebPKI group's implementation — the same
// people who wrote RFC 8785. That choice is deliberate and it is what makes the golden corpus worth
// anything: see TestGoldenCorpusMatchesRFCVectors for the chain of trust, which starts at the RFC's
// own published vectors and not at any of the three teams' code.
package stepidentity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"

	"github.com/gowebpki/jcs"
)

// Step is what the approval hash covers.
//
// The TRIPLE, settled on 2026-09-13 after Synaptum pointed out the hole: (step_id, tool_name,
// tool_args), and NOT node_id. node_id is Aeon's own and their replay cannot reproduce it, so a hash
// covering it could only be transported and believed — which is the one thing a hash exists to avoid.
// step_id also binds the approval to that step of that run rather than to that tool with those
// arguments at any point, which is a hole neither team had seen.
type Step struct {
	// StepID is the loop's own identifier for this step within its run.
	StepID string `json:"step_id"`
	// ToolName is the tool the step intends to call.
	ToolName string `json:"tool_name"`
	// ToolArgs is the arguments object, hashed structurally rather than as a pre-serialized string.
	// Hashing a string would make the hash depend on whoever serialized it first, which is the problem
	// again one level down.
	ToolArgs any `json:"tool_args"`
}

// Hash returns the hex-encoded SHA-256 of the step's canonical JSON.
//
// THE SHAPE HASHED IS A JSON OBJECT with exactly the three keys above, and the corpus pins that down
// because nothing else did. An object rather than an array on purpose: JCS sorts object keys, so the
// ordering is derived from the spec instead of from a convention the three teams would each have to
// remember — and a convention nobody can verify is how implementations drift apart while every test
// stays green.
func (s Step) Hash() (string, error) {
	canonical, err := s.CanonicalJSON()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// CanonicalJSON returns the exact bytes the hash is taken over.
//
// Exposed separately because it is the artifact worth comparing when two implementations disagree: two
// different hashes tell you nothing, two different byte sequences tell you where.
func (s Step) CanonicalJSON() ([]byte, error) {
	// The integer bound is checked FIRST, before anything is serialized. Afterwards the digit is gone and
	// there is nothing left to detect — see checkIntegerBounds.
	if err := checkIntegerBounds("tool_args", s.ToolArgs); err != nil {
		return nil, err
	}
	// Marshalled first and re-canonicalized, rather than hand-built: this way a nested value inside
	// ToolArgs goes through the same path as a top-level one, with no second code path to keep in step.
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("stepidentity: encoding step: %w", err)
	}
	canonical, err := jcs.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("stepidentity: canonicalizing step: %w", err)
	}
	return canonical, nil
}

// maxExactInteger is 2^53: the last CONSECUTIVE integer JCS round-trips exactly.
//
// The bound is a CLEAN CUT at this magnitude, not a round-trip check, and Synaptum agreed to the same on
// 2026-09-27 after measuring it themselves. The round-trip version is the one either of us would have
// written first — parse to a double, format it back, compare to the literal — and it is the trap: above
// 2^53 doubles step by two, so every EVEN integer up there still round-trips exactly.
//
//	2^53-1  exact     2^53    exact
//	2^53+1  FOLDS     2^53+2  exact      <- the trap
//	2^53+3  FOLDS     2^53+4  exact
//
// A round-trip check would therefore accept half the identifiers anyone uses, by parity. That is not a
// lax guard, it is worse than none: whoever integrates it watches their test values pass and concludes
// the matter is settled. So 2^53+2 is refused ALTHOUGH IT IS EXACT, because its neighbour is not and
// nobody can reason about the parity of an id that does not exist yet.
const maxExactInteger = 1 << 53

// ErrIntegerTooLargeToBind is returned when a step carries an integer no hash can safely bind.
type ErrIntegerTooLargeToBind struct {
	// Path is where the value sits inside the step, e.g. tool_args.payment.lines[0].id. Named because a
	// caller has to change THAT field: an error saying only "there is a large integer somewhere" makes
	// them hunt for it, and the hunt is the part that gets skipped.
	Path  string
	Value string
	// Float marks a value that arrived as a floating-point number, which is refused AT 2^53 rather than
	// above it. Reported because whoever gets a number refused that visibly "fits" will ask why, and the
	// answer is about provenance rather than size.
	Float bool
}

func (e *ErrIntegerTooLargeToBind) Error() string {
	if e.Float {
		return fmt.Sprintf("stepidentity: %s is the floating-point value %s, at or above 2^53 (%d). A float "+
			"at that magnitude cannot be bound EVEN IF IT LOOKS EXACT: 9007199254740993.0 becomes "+
			"9007199254740992.0 when parsed, so there is no way to tell which value was sent, and an "+
			"approval granted for one would validate the other. With an integer there is no ambiguity and "+
			"2^53 itself is accepted. If it is an identifier, send it as a string",
			e.Path, e.Value, int64(maxExactInteger))
	}
	return fmt.Sprintf("stepidentity: %s is %s, of magnitude greater than 2^53 (%d). A number that large "+
		"cannot be bound by this hash: RFC 8785 mandates ECMAScript number serialization and ECMAScript "+
		"numbers ARE IEEE-754 doubles, so two different values can canonicalize identically and an approval "+
		"granted for one would validate the other. If it is an identifier, send it as a string",
		e.Path, e.Value, int64(maxExactInteger))
}

// checkIntegerBounds walks the step's values BEFORE canonicalization.
//
// Before, because afterwards the digit is already gone and there is nothing left to detect. In Go the
// subtlety runs the opposite way from Python: encoding/json decodes a number into float64 unless the
// caller asked for json.Number, so by the time a big integer reaches here as a float64 it may ALREADY
// have been folded. That is why the bound is on the VALUE and not on whether the text survived: any
// magnitude above 2^53 is refused, folded or not, which is the same guarantee either way round.
func checkIntegerBounds(path string, v any) error {
	switch n := v.(type) {
	case json.Number:
		// Compared AS AN INTEGER, never via float64. The first version of this converted to float64 first
		// and then compared — and float64(9007199254740993) is 9007199254740992, so the guard folded the
		// value before judging it and ACCEPTED the single case it exists for. Measured: 2^53+1 passed.
		// The checker was destroyed by the very rounding it was written to catch.
		if i, err := n.Int64(); err == nil {
			return checkIntMagnitude(path, n.String(), i)
		}
		// Not an int64: either a fraction, or an integer too large to fit. Both are judged as FLOATS, which
		// also fixes a bug the first version had — it rejected every json.Number that was not an integer,
		// so a perfectly ordinary 1.5 was refused as "too large to bind". Found by printing the boundary
		// values rather than by reading this function.
		f, err := n.Float64()
		if err != nil {
			return nil // Not a number at all; JCS will reject it if it is malformed.
		}
		return checkFloatMagnitude(path, n.String(), f)
	case float64:
		return checkFloatMagnitude(path, formatBigFloat(n), n)
	case int:
		return checkIntMagnitude(path, strconv.Itoa(n), int64(n))
	case int64:
		return checkIntMagnitude(path, strconv.FormatInt(n, 10), n)
	case uint64:
		if n > maxExactInteger {
			return &ErrIntegerTooLargeToBind{Path: path, Value: strconv.FormatUint(n, 10)}
		}
	case map[string]any:
		// Sorted so the error names the same field every time for the same input. An unordered walk would
		// report whichever offending key came up first, and two runs would blame different fields.
		keys := make([]string, 0, len(n))
		for k := range n {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := checkIntegerBounds(path+"."+k, n[k]); err != nil {
				return err
			}
		}
	case []any:
		for i, item := range n {
			if err := checkIntegerBounds(fmt.Sprintf("%s[%d]", path, i), item); err != nil {
				return err
			}
		}
	}
	// bool is deliberately not handled: it is never an identifier. Nor are strings — sending a large id as
	// a string is exactly the escape this bound exists to push people toward.
	return nil
}

// checkIntMagnitude compares in int64, which is the whole point: no float64 anywhere on this path.
func checkIntMagnitude(path, literal string, v int64) error {
	if v > maxExactInteger || v < -maxExactInteger {
		return &ErrIntegerTooLargeToBind{Path: path, Value: literal}
	}
	return nil
}

// checkFloatMagnitude applies the FLOAT rule: at-or-above 2^53 is refused, not merely above it.
//
// INCLUSIVE, and the asymmetry with the integer rule is the point. Synaptum found this hole on their side
// on 2026-09-28 from the warning about our decoding asymmetry, and it was in this code too: a float
// literal 9007199254740993.0 becomes exactly 9007199254740992.0, which passes a `> 2^53` cut and gets
// hashed. Measured here before fixing.
//
// With an integer there is no ambiguity — 2^53 is 2^53. With a float there is no way to know where the
// value came from, and binding something we cannot distinguish from a different value is exactly what this
// check exists to prevent. So a float AT 2^53 is refused even though 2^53 itself is a legal bound: the
// refusal is about provenance, not magnitude.
//
// A genuine fraction below the bound passes, because the bound is about integers used as identifiers.
func checkFloatMagnitude(path, literal string, v float64) error {
	if v != math.Trunc(v) && math.Abs(v) < maxExactInteger {
		// A real fraction in the ordinary range: not an identifier, nothing to bind.
		return nil
	}
	if math.Abs(v) >= maxExactInteger {
		return &ErrIntegerTooLargeToBind{Path: path, Value: literal, Float: true}
	}
	return nil
}

// formatBigFloat renders a float for the error message without pasting three hundred digits into it.
//
// Scientific notation past int64 range: the exact digits of a folded float are not the useful part of the
// message — the field path is — and a screen of zeros buries it.
func formatBigFloat(v float64) string {
	if math.Abs(v) < 1e18 {
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}
