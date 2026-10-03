#!/usr/bin/env bash
# SEC-006: write deploy/compose/secrets/* for deploy/compose/secrets.override.yml.
#
# Three secrets, three different honest answers to "where does this value come from":
#
#   memory_hmac_key           OURS TO GENERATE. Nothing outside this deployment knows it; a fresh
#                             random value is strictly better than the committed dev default, which
#                             is public and which anyone can use to forge a provenance_hmac.
#   caller_token              OURS TO GENERATE, but it is half of a pair: the gateways verify its
#                             SHA-256 from the caller bundle, so generating one without updating the
#                             bundle produces a worker that 401s on every call. So this prints the
#                             hash and names the file to paste it into, and does not pretend.
#   prometheus_client_secret  NOT OURS. The platform shows it once at `POST /admin/clients` and it
#                             cannot be retrieved again. Migrated out of .env when it is there;
#                             otherwise this script says so and writes nothing.
#
# It never writes an empty file. An empty one would make `up` succeed and the service refuse to
# start, which is the right refusal in the wrong place — the message belongs here, where someone is
# reading it.
set -euo pipefail

cd "$(dirname "$0")/.."
DIR=deploy/compose/secrets
mkdir -p "$DIR"
chmod 700 "$DIR"

# Deliberately NOT sourced: `source .env` would run whatever is in it and would also pull every
# other variable into this shell. One grep per name, and nothing is echoed.
from_dotenv() {
  [ -f .env ] || return 0
  sed -n "s/^$1=\(.\{1,\}\)$/\1/p" .env | tail -n 1
}

write_secret() {
  local file="$DIR/$1" value="$2"
  if [ -z "$value" ]; then return 1; fi
  printf '%s' "$value" > "$file"
  chmod 600 "$file"
  return 0
}

generated_token=""

# --- memory_hmac_key ---------------------------------------------------------------------------
if [ -s "$DIR/memory_hmac_key" ]; then
  echo "kept      memory_hmac_key (already present; delete the file to regenerate)"
else
  value="$(from_dotenv AEON_MEMORY_HMAC_KEY)"
  if [ -n "$value" ]; then
    write_secret memory_hmac_key "$value" && echo "migrated  memory_hmac_key from .env"
  else
    write_secret memory_hmac_key "$(openssl rand -hex 32)" && echo "generated memory_hmac_key"
    echo "          NOTE: MEM-001 provenance_hmac values written under the previous key will no"
    echo "          longer verify. On a fresh database that is nothing; on one with memories it"
    echo "          makes every existing row look tampered with."
  fi
fi

# --- caller_token ------------------------------------------------------------------------------
if [ -s "$DIR/caller_token" ]; then
  echo "kept      caller_token (already present; delete the file to regenerate)"
else
  value="$(from_dotenv AEON_CALLER_TOKEN)"
  if [ -n "$value" ]; then
    write_secret caller_token "$value" && echo "migrated  caller_token from .env"
  else
    generated_token="$(openssl rand -hex 32)"
    write_secret caller_token "$generated_token" && echo "generated caller_token"
  fi
fi

# --- prometheus_client_secret ------------------------------------------------------------------
if [ -s "$DIR/prometheus_client_secret" ]; then
  echo "kept      prometheus_client_secret (already present)"
else
  value="$(from_dotenv PROMETHEUS_CLIENT_SECRET)"
  if write_secret prometheus_client_secret "$value"; then
    echo "migrated  prometheus_client_secret from .env"
  else
    echo "MISSING   prometheus_client_secret — not ours to generate: the Prometheus platform shows"
    echo "          the client_secret once at POST /admin/clients and cannot show it again."
    echo "          Either write it:   printf '%s' '<secret>' > $DIR/prometheus_client_secret"
    echo "          or, if this deployment has no Prometheus account, delete the"
    echo "          prometheus_client_secret entry and its four lines from"
    echo "          deploy/compose/secrets.override.yml."
  fi
fi

echo
echo "files in $DIR (sizes only — values are never printed):"
ls -l "$DIR" | awk 'NR>1 {printf "  %-28s %s bytes  %s\n", $NF, $5, $1}'

if [ -n "$generated_token" ]; then
  echo
  echo "THE CALLER TOKEN IS HALF A PAIR. The gateways verify its SHA-256 against the caller bundle"
  echo "(AEON_CALLERS_PATH), so this token does not work until the hash below is in that bundle's"
  echo "entry for the worker. Until then every call the worker makes gets a 401 that names"
  echo "AEON_CALLER_TOKEN, which reads as a missing credential rather than a stale one."
  echo
  echo "  tokenSHA256: $(printf '%s' "$generated_token" | shasum -a 256 | cut -d' ' -f1)"
  echo
  echo "Paste it into your own bundle — NOT examples/deep-research/callers.yaml, which is committed"
  echo "and whose tokens are public for exactly that reason (see .env.example, SEC-005)."
fi

echo
echo "Next, with the override in play:"
echo "  docker compose --env-file .env -f deploy/compose/docker-compose.yml \\"
echo "    -f deploy/compose/secrets.override.yml --profile core up -d --build"
echo
echo "AND THEN REMOVE THE VALUES FROM .env. Leaving both is not belt-and-braces: secretref refuses"
echo "to start a service that has <NAME> and <NAME>_FILE both set, because a rotation applied to one"
echo "of two sources is a rotation that silently did not happen. The override clears each <NAME> for"
echo "the services it covers, so the stack runs either way — but a value left in .env is still a"
echo "plaintext credential in a file on this host, which is the thing SEC-006 is about."
