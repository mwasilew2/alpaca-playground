#!/usr/bin/env bash
# otel-correlation-smoke.sh
#
# End-to-end smoke test proving that logs, metrics, and traces correlate.
#
# It boots the server with stdout OpenTelemetry exporters (no OTLP endpoint)
# pointed at a local stub "Alpaca" that returns an error quickly, drives a
# /bars request plus the watchlist poller, then triggers graceful shutdown so
# all three signal pipelines flush to stdout. Finally it extracts the trace id
# of the GET /bars request and shows that same id threading through the spans,
# the log records, and the metric exemplars.
#
# Requires: go, curl, python3, jq. Override APP_PORT / STUB_PORT if they clash.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

APP_PORT="${APP_PORT:-58231}"
STUB_PORT="${STUB_PORT:-58399}"
TMPDIR_RUN="$(mktemp -d)"
BIN="$TMPDIR_RUN/alpaca-smoke"
OUT="$TMPDIR_RUN/telemetry.jsonl"
APP_PID=""; STUB_PID=""

cleanup() {
  [ -n "$APP_PID" ] && kill "$APP_PID" 2>/dev/null
  [ -n "$STUB_PID" ] && kill "$STUB_PID" 2>/dev/null
  wait 2>/dev/null
}
trap cleanup EXIT

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing required tool: $1" >&2; exit 1; }; }
need go; need curl; need python3; need jq

echo "==> building"
go build -o "$BIN" . || exit 1

echo "==> starting stub Alpaca on :$STUB_PORT (returns 403 quickly)"
python3 - "$STUB_PORT" <<'PY' &
import sys, http.server
port = int(sys.argv[1])
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(403)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b'{"message":"forbidden"}')
    def log_message(self, *a):
        pass
http.server.HTTPServer(("127.0.0.1", port), H).serve_forever()
PY
STUB_PID=$!

echo "==> starting app on :$APP_PORT (stdout OTel exporters)"
ALPACA_BASE_URL="http://127.0.0.1:$STUB_PORT" \
ALPACA_API_KEY=smoke ALPACA_API_SECRET=smoke \
PORT="$APP_PORT" PPROF_ADDR=- WATCHLIST=AAPL POLL_INTERVAL=3s \
  "$BIN" >"$OUT" 2>&1 &
APP_PID=$!

for _ in $(seq 1 40); do
  curl -sf -m 1 "http://127.0.0.1:$APP_PORT/healthz" >/dev/null 2>&1 && break
  sleep 0.25
done

echo "==> driving requests"
curl -s -m 8 "http://127.0.0.1:$APP_PORT/bars?symbol=AAPL&range=10m" -o /dev/null -w "    GET /bars    -> %{http_code}\n"
curl -s -m 5 "http://127.0.0.1:$APP_PORT/healthz"                    -o /dev/null -w "    GET /healthz -> %{http_code}\n"
sleep 1

echo "==> shutting down (flushes traces/logs/metrics)"
kill -INT "$APP_PID"; wait "$APP_PID" 2>/dev/null
APP_PID=""

# b64_to_hex decodes a base64 trace/span id (how metric exemplars encode them)
# into the lowercase hex that spans and logs use, so they can be compared.
b64_to_hex() { printf '%s' "$1" | base64 -d 2>/dev/null | od -An -tx1 | tr -d ' \n'; }

echo
echo "================= CORRELATION REPORT ================="

# Anchor on the failed /bars request via the trace id stamped on its error log.
TID="$(jq -r 'select(.Body?.Value=="bars fetch failed") | .TraceID' "$OUT" 2>/dev/null | head -1)"
if [ -z "$TID" ] || [ "$TID" = "null" ]; then
  TID="$(jq -r 'select(.Name?=="GET /bars") | .SpanContext.TraceID' "$OUT" 2>/dev/null | head -1)"
fi

if [ -z "$TID" ] || [ "$TID" = "null" ]; then
  echo "Could not locate a /bars trace id — inspect the dump below."
else
  echo "Anchor trace id (the GET /bars request): $TID"
  echo
  echo "TRACES — spans sharing this trace id:"
  jq -r --arg t "$TID" '
    select(.SpanContext?.TraceID==$t)
    | "  - " + .Name + "   (span " + .SpanContext.SpanID + ", parent " + (.Parent.SpanID // "-") + ")"' "$OUT" | sort -u

  echo
  echo "LOGS - records stamped with this trace id:"
  jq -r --arg t "$TID" '
    select(.TraceID?==$t and .Body?!=null)
    | "  - [" + (.SeverityText // "?") + "] " + (.Body.Value|tostring) + "   (span " + .SpanID + ")"' "$OUT"

  echo
  echo "METRICS - instruments whose data-point exemplars point back to this trace:"
  jq -r '
    .ScopeMetrics[]?.Metrics[]? as $m
    | ($m.Data.DataPoints[]?.Exemplars[]? | [$m.Name, .TraceID] | @tsv)' "$OUT" 2>/dev/null \
  | while IFS=$'\t' read -r mname b64; do
      [ "$(b64_to_hex "$b64")" = "$TID" ] && echo "  - $mname"
    done | sort -u
fi

echo
echo "The watchlist poller runs as its own trace: look for the \"poller refresh"
echo "failed\" logs and poller.* exemplars, which share the poller.refresh span's"
echo "trace id the same way."
echo
echo "Full telemetry dump: $OUT"
