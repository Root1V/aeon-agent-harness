#!/usr/bin/env bash
# Starts examples/first-use-case against a running stack and reports what happened.
#
# Everything here is a real HTTP call against the real Run Controller and the real Tool Gateway: no
# fixture, no stand-in. The last check is the one worth reading — a tool the policy FORBIDS is refused,
# and the refusal names the policy that refused it.
set -euo pipefail

RUNCONTROLLER=${RUNCONTROLLER:-127.0.0.1:9404}
TOOLGW=${TOOLGW:-127.0.0.1:9403}
TOKEN=${AEON_CALLER_TOKEN:-dev-first-use-case-token-not-a-secret}
AGENT=first-use-case@0.1.0
RUN_ID="first-use-case-$(date +%s)"
GRAPH=$(cat "$(dirname "$0")/../examples/first-use-case/graph.json")

say() { printf '\n%s\n' "$1"; }

say "1. Starting the run as $AGENT"
code=$(curl -s -o /tmp/aeon-first-run.json -w '%{http_code}' -X POST "http://$RUNCONTROLLER/runs" \
  -H 'Content-Type: application/json' -H "Authorization: Bearer $TOKEN" \
  -d "{\"run_id\":\"$RUN_ID\",\"agent_manifest_ref\":\"$AGENT\",\"graph\":$GRAPH}")
if [ "$code" != "201" ]; then
  echo "POST /runs returned $code:" >&2; cat /tmp/aeon-first-run.json >&2; exit 1
fi
echo "   run_id = $RUN_ID"

say "2. Waiting for it to finish"
for _ in $(seq 1 60); do
  status=$(curl -s -H "Authorization: Bearer $TOKEN" "http://$RUNCONTROLLER/runs/$RUN_ID" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])')
  [ "$status" = "RUNNING" ] || break
  sleep 1
done
curl -s -H "Authorization: Bearer $TOKEN" "http://$RUNCONTROLLER/runs/$RUN_ID" \
  | python3 -c 'import json,sys; d=json.load(sys.stdin); print("   status =", d["status"], " tool_calls =", d["budgets_consumed"]["tool_calls"])'
if [ "$status" != "SUCCEEDED" ]; then
  echo "the run did not succeed (status=$status) — 'make logs' or docker logs aeon-worker-1" >&2; exit 1
fi

say "3. The tools really ran: reading the same file directly through the gateway"
curl -s -X POST "http://$TOOLGW/execute" -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $TOKEN" \
  -d "{\"agent_manifest_ref\":\"$AGENT\",\"tool_name\":\"repository.read\",\"args\":{\"path\":\"adr/0001-temporal-determinism-boundary.md\",\"start_line\":1,\"end_line\":1}}" \
  | python3 -c 'import json,sys; d=json.load(sys.stdin); print("   allowed =", d.get("allowed"), "| policy =", d.get("policy_id"), "\n   content =", repr(d.get("result",{}).get("content","")[:70]))'

say "4. And a tool the policy forbids is refused, by name"
curl -s -X POST "http://$TOOLGW/execute" -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $TOKEN" \
  -d "{\"agent_manifest_ref\":\"$AGENT\",\"tool_name\":\"shell.exec\",\"args\":{\"command\":\"id\"}}" \
  | python3 -c 'import json,sys; d=json.load(sys.stdin); print("   allowed =", d.get("allowed"), "| policy =", d.get("policy_id"), "| disposition =", d.get("disposition"))'
