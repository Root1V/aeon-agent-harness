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
