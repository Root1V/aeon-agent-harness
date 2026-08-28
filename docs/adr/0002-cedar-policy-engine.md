# ADR-002: Cedar as the Policy Engine

## Status
Accepted

## Context
SEC-001 requires authorization to happen outside the model, after tool arguments are generated and
before execution. Candidates considered: Open Policy Agent (Rego) and Cedar (AWS, open-sourced,
purpose-built for application-level authorization — see
https://aws.amazon.com/blogs/security/enforce-least-privilege-authorization-in-multi-agent-ai-chains-using-cedar/).

## Decision
We use Cedar. Reasons:
- It targets exactly our shape of problem — principal (agent + run + tenant) / action (tool call) /
  resource (data scope) — rather than being a general-purpose query language like Rego.
- It is statically analyzable: policies can be checked for certain classes of contradiction or
  overly-broad grants before deployment, which matters for a least-privilege story we want to be
  able to audit, not just test.
- There is a published reference architecture for exactly our use case: least-privilege
  authorization in multi-agent delegation chains (AWS sample:
  aws-samples/sample-cedar-agentic-ai-authorization), addressing OWASP ASI03 (Identity & Privilege
  Abuse) directly.

## Consequences
- `PolicyBundle` manifests (proto/manifests/policy_bundle.schema.json) carry raw Cedar policy text,
  versioned in Git like everything else (FND-003).
- The Tool Gateway is the only component that evaluates Cedar policies; agents and the SDK never
  see or influence policy evaluation directly.
- If a future requirement needs Rego-style general-purpose querying (e.g. complex data-residency
  rules spanning non-agent systems), that is a second, separate policy surface — not a reason to
  replace Cedar for tool/agent authorization.
