package policy

import "fmt"

// Disposition is what the enforcement seam tells the loop to DO, as opposed to whether the call was
// allowed (INT-010).
//
// The four values are SYNAPTUM'S, published by them on 2026-09-27 and not designed here. That matters
// more than it looks: it is their loop that acts on the disposition, so the vocabulary is theirs, and an
// earlier version of this row had three values I had invented by deduction. Three of the four happened to
// be right, which was luck and not validation — they checked and found `terminate_run` appeared zero
// times in the shared agreements file, so their own seam's vocabulary had lived only in their code while
// the team implementing it guessed.
type Disposition string

const (
	// DispositionAllow: the call may proceed.
	DispositionAllow Disposition = "allow"
	// DispositionDenyStep: this effect may not happen, and the loop MAY TRY SOMETHING ELSE. It goes back
	// to the model as evidence, and a model that sees the refusal usually corrects course — which is the
	// point of it being distinct from terminating.
	DispositionDenyStep Disposition = "deny_step"
	// DispositionTerminateRun: there is no next turn. Distinct from deny_step because they are two
	// different points of the loop, not a shade of the same one: collapsing them would turn a hard policy
	// into an infinite retry against a closed door.
	DispositionTerminateRun Disposition = "terminate_run"
	// DispositionRequireApproval: a person must decide first. The run SUSPENDS rather than failing.
	//
	// It belongs to the seam and not to the loop, and the order is the reason: whoever governs decides
	// that a person is needed, and the loop emits its ApprovalStep as a CONSEQUENCE. The other way round,
	// the framework would be deciding when to ask — which is exactly what does not belong to it.
	DispositionRequireApproval Disposition = "require_approval"
)

// There is deliberately no `retry_later`, and Synaptum's argument for leaving it out is the one recorded
// here because it is better than the one I would have given: retryability travels in the ERROR TAXONOMY
// (`retryable`, `retry_after`, which this codebase already honours), not in a policy decision. "You may
// not do this" is firm; "not right now" is transitory. Putting both in one enum would make the loop ask
// the same question in two places, and the day they disagreed the winner would be whichever was read
// first.

// PermitsExecution reports whether this disposition lets the call happen NOW.
//
// require_approval answers FALSE here, and that is the single most important line in this file. Every
// existing call site of Decision reads `if !decision.Allowed { refuse }`; if a require_approval decision
// left Allowed true, all of them would execute the effect without ever asking anybody. So the seam fails
// closed by construction, and only code that understands dispositions can act on the difference between
// "refused" and "refused until a person says yes".
func (d Disposition) PermitsExecution() bool { return d == DispositionAllow }

// Terminal reports whether the run is over.
func (d Disposition) Terminal() bool { return d == DispositionTerminateRun }

// Valid reports whether d is one of the four.
func (d Disposition) Valid() bool {
	switch d {
	case DispositionAllow, DispositionDenyStep, DispositionTerminateRun, DispositionRequireApproval:
		return true
	default:
		return false
	}
}

// strictness orders the dispositions so the strictest among several matching policies wins.
//
// Needed because Cedar can report MORE THAN ONE determining policy. If two permits match and only one
// says a person must approve, honouring the other would be the comfortable error — the call runs and
// nobody is asked. Same on the denial side: a forbid that terminates the run outranks one that only
// refuses the step.
func (d Disposition) strictness() int {
	switch d {
	case DispositionAllow:
		return 0
	case DispositionRequireApproval:
		return 1
	case DispositionDenyStep:
		return 2
	case DispositionTerminateRun:
		return 3
	default:
		return -1
	}
}

// stricter returns whichever of the two constrains more.
func stricter(a, b Disposition) Disposition {
	if b.strictness() > a.strictness() {
		return b
	}
	return a
}

// validateDeclaredDisposition checks that a bundle item's declared disposition agrees with its effect.
//
// A `forbid` declaring `allow`, or a `permit` declaring `terminate_run`, is a contradiction, and a
// contradiction has to fail at LOAD time: by the time it is evaluated the bundle is already in force, and
// whichever half of the contradiction the code happened to honour would become the policy.
func validateDeclaredDisposition(effect string, d Disposition) error {
	if d == "" {
		return nil
	}
	if !d.Valid() {
		return fmt.Errorf("disposition %q is not one of %s/%s/%s/%s",
			d, DispositionAllow, DispositionDenyStep, DispositionTerminateRun, DispositionRequireApproval)
	}
	switch effect {
	case "forbid":
		if d == DispositionAllow || d == DispositionRequireApproval {
			return fmt.Errorf("a forbid policy cannot declare disposition %q: it would say the call is refused and permitted at once", d)
		}
	case "permit":
		if d == DispositionDenyStep || d == DispositionTerminateRun {
			return fmt.Errorf("a permit policy cannot declare disposition %q: a policy that refuses is a forbid, and writing it as a permit hides that from anyone reading the bundle", d)
		}
	}
	return nil
}

// derivedDisposition is what a decision means when no policy declared anything.
//
// NOT a guess, and the distinction is what makes the default acceptable. `deny_step` is the faithful
// translation of what a Cedar `forbid` actually says — "this call may not happen" — and nothing more.
// `terminate_run` asserts something Cedar does not say, so a bundle has to opt into it. Same on the other
// side: `allow` is what a `permit` says, and `require_approval` adds to it.
//
// Whether it was declared or derived travels on the Decision, because "nobody has thought about this
// policy's disposition" and "somebody decided deny_step" are different facts about a bundle.
func derivedDisposition(allowed bool) Disposition {
	if allowed {
		return DispositionAllow
	}
	return DispositionDenyStep
}
