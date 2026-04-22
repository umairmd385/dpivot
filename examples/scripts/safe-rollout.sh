#!/usr/bin/env bash
# safe-rollout.sh — Zero-downtime deploy with automatic rollback on failure.
#
# Usage:
#   ./examples/scripts/safe-rollout.sh <service> [compose-file]
#
# What it does:
#   1. Validates the new image's health endpoint before switching traffic
#   2. Runs dpivot rollout (scales +1, waits for healthcheck, switches traffic)
#   3. Monitors the new deployment for VERIFY_SECONDS after cutover
#   4. Automatically rolls back if error rate spikes or health check fails
#
# Environment variables:
#   DPIVOT_API_TOKEN   Bearer token for the control API (optional)
#   CONTROL_ADDR       dpivot control API address (default: http://localhost:9900)
#   VERIFY_SECONDS     Seconds to monitor after cutover before declaring success (default: 30)
#   ERROR_THRESHOLD    Max acceptable failed connections during verify window (default: 3)

set -euo pipefail

SERVICE="${1:?Usage: $0 <service> [compose-file]}"
COMPOSE_FILE="${2:-dpivot-compose.yml}"
CONTROL_ADDR="${CONTROL_ADDR:-http://localhost:9900}"
VERIFY_SECONDS="${VERIFY_SECONDS:-30}"
ERROR_THRESHOLD="${ERROR_THRESHOLD:-3}"
AUTH_HEADER=""
if [[ -n "${DPIVOT_API_TOKEN:-}" ]]; then
  AUTH_HEADER="Authorization: Bearer ${DPIVOT_API_TOKEN}"
fi

log() { echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] $*" >&2; }
die() { log "ERROR: $*"; exit 1; }

# ── Helpers ───────────────────────────────────────────────────────────────────

control_get() {
  local path="$1"
  if [[ -n "$AUTH_HEADER" ]]; then
    curl -sf -H "$AUTH_HEADER" "${CONTROL_ADDR}${path}"
  else
    curl -sf "${CONTROL_ADDR}${path}"
  fi
}

get_failed_conns() {
  control_get /metrics 2>/dev/null \
    | grep '^dpivot_connections_failed_total ' \
    | awk '{print $2}' \
    || echo 0
}

get_active_backends() {
  control_get /metrics 2>/dev/null \
    | grep '^dpivot_backends_active ' \
    | awk '{print $2}' \
    || echo 0
}

# ── Pre-flight checks ─────────────────────────────────────────────────────────

log "==> Starting safe rollout: service=${SERVICE}"

# Verify the proxy control API is reachable.
control_get /health/live > /dev/null \
  || die "Proxy control API unreachable at ${CONTROL_ADDR}. Is the stack running?"

# Snapshot baseline metrics before the rollout.
BASELINE_ERRORS=$(get_failed_conns)
log "Baseline failed connections: ${BASELINE_ERRORS}"

# ── Run rollout ───────────────────────────────────────────────────────────────

log "==> Running dpivot rollout..."
if ! dpivot rollout "${SERVICE}" \
    --file "${COMPOSE_FILE}" \
    --control-addr "${CONTROL_ADDR}" \
    ${DPIVOT_API_TOKEN:+--api-token "${DPIVOT_API_TOKEN}"}; then
  die "dpivot rollout failed — no traffic was switched (pre-healthcheck failure)"
fi

log "==> Rollout complete. Monitoring for ${VERIFY_SECONDS}s..."

# ── Post-rollout verification ─────────────────────────────────────────────────

START_TIME=$(date +%s)
ROLLBACK_TRIGGERED=false

while true; do
  NOW=$(date +%s)
  ELAPSED=$(( NOW - START_TIME ))

  if [[ "$ELAPSED" -ge "$VERIFY_SECONDS" ]]; then
    log "==> Verification window passed (${VERIFY_SECONDS}s). Rollout successful."
    break
  fi

  # Check active backends.
  ACTIVE=$(get_active_backends)
  if [[ "$ACTIVE" -eq 0 ]]; then
    log "WARN: No active backends after rollout — triggering rollback"
    ROLLBACK_TRIGGERED=true
    break
  fi

  # Check error rate since rollout started.
  CURRENT_ERRORS=$(get_failed_conns)
  NEW_ERRORS=$(( CURRENT_ERRORS - BASELINE_ERRORS ))
  if [[ "$NEW_ERRORS" -gt "$ERROR_THRESHOLD" ]]; then
    log "WARN: ${NEW_ERRORS} new failed connections (threshold: ${ERROR_THRESHOLD}) — triggering rollback"
    ROLLBACK_TRIGGERED=true
    break
  fi

  log "  ${ELAPSED}s/${VERIFY_SECONDS}s — backends=${ACTIVE}, new_errors=${NEW_ERRORS}"
  sleep 5
done

# ── Rollback if needed ────────────────────────────────────────────────────────

if [[ "$ROLLBACK_TRIGGERED" == true ]]; then
  log "==> ROLLING BACK ${SERVICE}..."
  if dpivot rollback "${SERVICE}" \
      --control-addr "${CONTROL_ADDR}" \
      ${DPIVOT_API_TOKEN:+--api-token "${DPIVOT_API_TOKEN}"}; then
    log "==> Rollback complete. Previous version restored."
    exit 2   # exit 2 = rollback performed (distinct from 0=success, 1=error)
  else
    die "Rollback command failed. Manual intervention required. Check: dpivot status"
  fi
fi

log "==> Deploy verified. Service ${SERVICE} is healthy."
exit 0
