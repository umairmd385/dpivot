#!/usr/bin/env bash
# rollback.sh — Manual rollback helper for a dpivot-managed service.
#
# Usage:
#   ./examples/scripts/rollback.sh <service>
#
# This script is a thin wrapper around `dpivot rollback` that adds:
#   - Pre-rollback status report
#   - Post-rollback health verification
#   - Clear exit codes for CI/CD pipeline integration
#
# Exit codes:
#   0 — rollback succeeded and service is healthy
#   1 — rollback failed or service still unhealthy after rollback
#
# When to use:
#   dpivot rollout saves a state file at /tmp/dpivot-<service>-state.json
#   between the moment the new backend is registered and the old one is removed.
#   Run this script immediately when you notice the new deployment is broken.

set -euo pipefail

SERVICE="${1:?Usage: $0 <service>}"
CONTROL_ADDR="${CONTROL_ADDR:-http://localhost:9900}"
HEALTH_CHECK_RETRIES="${HEALTH_CHECK_RETRIES:-10}"
AUTH_HEADER=""
if [[ -n "${DPIVOT_API_TOKEN:-}" ]]; then
  AUTH_HEADER="Authorization: Bearer ${DPIVOT_API_TOKEN}"
fi

log()  { echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] $*" >&2; }
die()  { log "ERROR: $*"; exit 1; }
warn() { log "WARN:  $*" >&2; }

control_get() {
  local path="$1"
  if [[ -n "$AUTH_HEADER" ]]; then
    curl -sf --max-time 5 -H "$AUTH_HEADER" "${CONTROL_ADDR}${path}"
  else
    curl -sf --max-time 5 "${CONTROL_ADDR}${path}"
  fi
}

# ── Pre-rollback status ───────────────────────────────────────────────────────

log "==> Pre-rollback state for service: ${SERVICE}"

# Show current backends.
log "Current backends:"
control_get /backends 2>/dev/null | python3 -m json.tool 2>/dev/null || \
  control_get /backends 2>/dev/null || warn "Could not query backends"

# Show current metrics.
log "Current error count:"
control_get /metrics 2>/dev/null \
  | grep 'dpivot_connections_failed_total\|dpivot_backends_active' || true

# Verify state file exists.
STATE_FILE="/tmp/dpivot-${SERVICE}-state.json"
if [[ ! -f "$STATE_FILE" ]]; then
  die "No rollout state found at ${STATE_FILE}. Cannot roll back. Check: dpivot status"
fi

log "Rollout state found:"
cat "$STATE_FILE" | python3 -m json.tool 2>/dev/null || cat "$STATE_FILE"
echo

# ── Execute rollback ──────────────────────────────────────────────────────────

log "==> Executing rollback..."
dpivot rollback "${SERVICE}" \
  --control-addr "${CONTROL_ADDR}" \
  ${DPIVOT_API_TOKEN:+--api-token "${DPIVOT_API_TOKEN}"} \
  || die "dpivot rollback command failed"

log "==> Rollback command completed. Verifying service health..."

# ── Post-rollback health check ────────────────────────────────────────────────

for i in $(seq 1 "$HEALTH_CHECK_RETRIES"); do
  ACTIVE=$(control_get /metrics 2>/dev/null \
    | grep '^dpivot_backends_active ' | awk '{print $2}' || echo 0)

  if [[ "$ACTIVE" -gt 0 ]]; then
    log "==> Service ${SERVICE} is healthy (${ACTIVE} active backend(s))."

    log "Post-rollback backends:"
    control_get /backends 2>/dev/null | python3 -m json.tool 2>/dev/null || \
      control_get /backends 2>/dev/null || true

    log "==> Rollback successful."
    exit 0
  fi

  log "  Attempt ${i}/${HEALTH_CHECK_RETRIES}: no active backends yet, retrying..."
  sleep 3
done

die "Service ${SERVICE} has no active backends after rollback. Manual check required: dpivot status"
