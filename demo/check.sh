#!/usr/bin/env bash
# Checks a running demo from outside: the gateway's metrics pass Prometheus's own linter,
# Prometheus has every number the dashboard shows, Grafana has the dashboard and Jaeger has
# the traces. CI runs this after `docker compose up`; you can too.
set -euo pipefail

cd "$(dirname "$0")"
gateway="http://127.0.0.1:${DEMO_GATEWAY_PORT:-8080}"
prometheus="http://127.0.0.1:${DEMO_PROMETHEUS_PORT:-9090}"
grafana="http://127.0.0.1:${DEMO_GRAFANA_PORT:-3000}"
jaeger="http://127.0.0.1:${DEMO_JAEGER_PORT:-16686}"

# wait_for <seconds> <what> <command...> runs the command once a second until it succeeds.
wait_for() {
  local limit=$1 what=$2
  shift 2
  for ((i = 0; i < limit; i++)); do
    if "$@" >/dev/null 2>&1; then
      echo "ok: $what"
      return 0
    fi
    sleep 1
  done
  echo "FAILED after ${limit}s: $what" >&2
  return 1
}

# above_zero <promql> succeeds when the query has a result greater than zero.
above_zero() {
  curl -fsS "$prometheus/api/v1/query" --data-urlencode "query=($1) > 0" | grep -q '"result":\[{'
}

status() {
  curl -s -o /dev/null -w '%{http_code}' "$@"
}

wait_for 60 "Prometheus has answered requests" above_zero 'sum(transponder_requests_total{code="200"})'
wait_for 60 "Prometheus has Responses API calls" above_zero 'sum(transponder_requests_total{endpoint="responses"})'
wait_for 30 "Prometheus has request durations" above_zero 'sum(transponder_request_duration_seconds_count)'
wait_for 30 "Prometheus has tokens" above_zero 'sum(transponder_tokens_total)'
wait_for 30 "Prometheus has cost" above_zero 'sum(transponder_cost_usd_total)'
wait_for 30 "Prometheus has provider latency" above_zero 'sum(transponder_upstream_duration_seconds_count)'
wait_for 120 "Prometheus has fallbacks" above_zero 'sum(transponder_fallbacks_total)'
wait_for 120 "Prometheus has calls over a rate" above_zero 'sum(transponder_limited_total{reason="rate_limited"})'
wait_for 180 "Prometheus has calls over a budget" above_zero 'sum(transponder_limited_total{reason="budget_exceeded"})'

[ "$(status "$gateway/metrics")" = 401 ] || { echo "FAILED: /metrics on the gateway's port answers without a key" >&2; exit 1; }
echo "ok: /metrics on the gateway's port needs a key"

curl -fsS -H "Authorization: Bearer tp_demo_viewer" "$gateway/metrics" |
  docker compose exec -T prometheus promtool check metrics
echo "ok: promtool accepts the gateway's metrics"

wait_for 30 "Grafana has the dashboard" curl -fsS "$grafana/api/dashboards/uid/transponder"
wait_for 60 "Jaeger has traces from transponder" bash -c "curl -fsS '$jaeger/api/v3/services' | grep -q '\"transponder\"'"

echo "the demo works"
