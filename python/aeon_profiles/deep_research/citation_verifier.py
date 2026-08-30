"""Citation Verifier (DR-005): a second, independent, read-only check on a Reporter draft (DR-004)
before it can be published — defense in depth, not a duplicate of DR-004's build-time gate (the same
"verificación redundante" philosophy as RUN-003's dual budget enforcement). DR-004 already stops a
*freshly generated* draft from citing outside its allowed set; this module instead re-checks
whatever draft it's actually handed — which could have come from a different path, or the Evidence
Ledger could have changed underneath it — against the Ledger one more time.

For a citation that doesn't resolve to an allowed claim, this module attempts to repair it: if the
draft's own prose already contains (verbatim) the quote of some other, genuinely allowed claim, the
citation is corrected to point there instead of being thrown away — a very real LLM failure mode is
writing accurate content while hallucinating which claim_id label it belongs to. Repair only ever
substitutes a REAL claim_id already in the ledger; it never fabricates or waves through evidence
that doesn't exist. A citation with no such match is a hard, unrepaired rejection.
"""
from __future__ import annotations

from dataclasses import dataclass, field

from aeon_evidence.ledger import EvidenceLedger
from aeon_profiles.deep_research.reporter import ReportDraft, select_allowed_claims
from aeon_profiles.deep_research.sufficiency_gate import SufficiencyDecision


@dataclass
class CitationIssue:
    claim_id: str
    repaired_with: str | None = None  # a real, allowed claim_id, if repair found one


@dataclass
class VerificationResult:
    verified: bool
    cited_claim_ids: list[str]  # the final list — repaired citations replaced, in original order
    issues: list[CitationIssue] = field(default_factory=list)


def verify_and_repair(draft: ReportDraft, decision: SufficiencyDecision, ledger: EvidenceLedger) -> VerificationResult:
    allowed_claims = select_allowed_claims(decision, ledger)
    allowed_by_id = {c["claim_id"]: c for c in allowed_claims}

    issues: list[CitationIssue] = []
    final_ids: list[str] = []
    used_repairs: set[str] = set()

    for claim_id in draft.cited_claim_ids:
        if claim_id in allowed_by_id:
            final_ids.append(claim_id)
            continue

        repair = _find_repair_candidate(draft.text, allowed_claims, used_repairs)
        if repair is not None:
            used_repairs.add(repair["claim_id"])
            issues.append(CitationIssue(claim_id=claim_id, repaired_with=repair["claim_id"]))
            final_ids.append(repair["claim_id"])
            continue

        issues.append(CitationIssue(claim_id=claim_id, repaired_with=None))

    unrepairable = [i for i in issues if i.repaired_with is None]
    return VerificationResult(verified=not unrepairable, cited_claim_ids=final_ids, issues=issues)


def _find_repair_candidate(text: str, allowed_claims: list[dict], used: set[str]) -> dict | None:
    """A deterministic, non-fabricating repair heuristic: the first allowed, not-yet-used claim
    whose exact quote (unquoted) appears verbatim in the draft's prose. No semantic matching, no
    invented content — only a real claim already present in the ledger, and only when the report's
    own text already demonstrates it. `used` avoids collapsing two distinct invented citations onto
    the same real claim when a second, still-unused real match exists."""
    for claim in allowed_claims:
        if claim["claim_id"] not in used and claim["quote"].strip('"') in text:
            return claim
    return None
