#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENGINE="${CONTAINER_ENGINE:-docker}"
IMAGE="${IMAGE:-scribe:local}"
LEGACY_PORT="${SCRIBE_SMOKE_LEGACY_PORT:-18443}"
SCRIBE_PORT="${SCRIBE_SMOKE_PORT:-18444}"
CONTAINER_NAME="scribe-smoke-$$-${RANDOM}"
UPSTREAM_PID=""

for command in "$ENGINE" openssl python3 curl grep; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "required command not found: $command" >&2
    exit 1
  }
done

cd "$ROOT"
"$ENGINE" compose -f deploy/compose.yaml config --no-env-resolution --quiet
"$ENGINE" build -f Containerfile -t "$IMAGE" .

SMOKE_DIR="$(mktemp -d "${TMPDIR:-/tmp}/scribe-container-smoke.XXXXXX")"
cleanup() {
  "$ENGINE" rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
  if [[ -n "$UPSTREAM_PID" ]]; then
    kill "$UPSTREAM_PID" >/dev/null 2>&1 || true
    wait "$UPSTREAM_PID" 2>/dev/null || true
  fi
  rm -rf "$SMOKE_DIR"
}
trap cleanup EXIT

openssl req -x509 -newkey rsa:2048 -nodes \
  -keyout "$SMOKE_DIR/key.pem" -out "$SMOKE_DIR/cert.pem" -days 1 \
  -subj "/CN=localhost" -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" \
  >"$SMOKE_DIR/openssl.log" 2>&1

python3 - "$SMOKE_DIR/cert.pem" "$SMOKE_DIR/key.pem" "$LEGACY_PORT" \
  >"$SMOKE_DIR/upstream.log" 2>&1 <<'PY' &
import http.server
import ssl
import sys

calls = 0

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.respond()

    def do_POST(self):
        self.respond()

    def respond(self):
        global calls
        calls += 1
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length).decode()
        response = f"{self.command} {self.path} {body}".encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(response)))
        self.send_header("X-Upstream", "legacy")
        self.send_header("X-Upstream-Calls", str(calls))
        self.end_headers()
        self.wfile.write(response)

    def log_message(self, format, *args):
        pass

server = http.server.HTTPServer(("127.0.0.1", int(sys.argv[3])), Handler)
context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
context.load_cert_chain(sys.argv[1], sys.argv[2])
server.socket = context.wrap_socket(server.socket, server_side=True)
server.serve_forever()
PY
UPSTREAM_PID=$!

"$ENGINE" run -d --rm --name "$CONTAINER_NAME" --network host \
  --read-only --cap-drop=ALL --security-opt=no-new-privileges:true \
  --tmpfs /tmp:rw,noexec,nosuid,size=16m \
  -e SCRIBE_LISTEN_ADDR="127.0.0.1:$SCRIBE_PORT" \
  -e SCRIBE_PUBLIC_BASE_URL="https://localhost:$SCRIBE_PORT" \
  -e SCRIBE_LEGACY_BASE_URL="https://localhost:$LEGACY_PORT" \
  -e SCRIBE_PURE_PROXY=true \
  -e SCRIBE_REDIS_URL=rediss://127.0.0.1:1/0 \
  -e SSL_CERT_FILE=/tmp/ca-certificates.crt \
  -v "$SMOKE_DIR/cert.pem:/tmp/ca-certificates.crt:ro" \
  "$IMAGE" >/dev/null

CURL=(curl --noproxy '*' --retry 20 --retry-connrefused --retry-delay 1 --retry-max-time 30 -sS)
HEALTH="$("${CURL[@]}" -f "http://127.0.0.1:$SCRIBE_PORT/healthz")"
READY="$("${CURL[@]}" -f "http://127.0.0.1:$SCRIBE_PORT/readyz")"
[[ "$HEALTH" == '{"status":"ok"}' ]]
[[ "$READY" == '{"status":"ok"}' ]]

expect_proxy() {
  local method="$1" path="$2" body="${3:-}"
  local headers="$SMOKE_DIR/headers" response="$SMOKE_DIR/response"
  "${CURL[@]}" -f -D "$headers" -o "$response" -X "$method" \
    --data-binary "$body" "http://127.0.0.1:$SCRIBE_PORT$path"
  [[ "$(<"$response")" == "$method $path $body" ]]
  grep -Fqi 'X-Upstream: legacy' "$headers"
  grep -Fqi "X-Authlib-Injector-API-Location: https://localhost:$SCRIBE_PORT" "$headers"
}

expect_proxy GET /
expect_proxy POST /authserver/authenticate not-json
expect_proxy POST /authserver/refresh '{"accessToken":"token-canary"}'
expect_proxy POST /authserver/validate '{"accessToken":"token-canary"}'
expect_proxy POST /authserver/invalidate '{"accessToken":"token-canary"}'
expect_proxy POST /authserver/signout '{"username":"smoke-user","password":"password-canary"}'
grep -Fqi 'X-Upstream-Calls: 6' "$SMOKE_DIR/headers"

STATUS="$("${CURL[@]}" -o "$SMOKE_DIR/internal" -w '%{http_code}' \
  "http://127.0.0.1:$SCRIBE_PORT/internal/migrations/commit")"
[[ "$STATUS" == 404 ]]

USER_ID="$("$ENGINE" inspect --format '{{.Config.User}}' "$CONTAINER_NAME")"
READ_ONLY="$("$ENGINE" inspect --format '{{.HostConfig.ReadonlyRootfs}}' "$CONTAINER_NAME")"
[[ "$USER_ID" == 65532 || "$USER_ID" == 65532:* ]]
[[ "$READ_ONLY" == true ]]

"$ENGINE" logs "$CONTAINER_NAME" >"$SMOKE_DIR/scribe.log" 2>&1
if grep -Eqi 'token-canary|password-canary|smoke-user' "$SMOKE_DIR/scribe.log"; then
  echo "sensitive request data leaked into container logs" >&2
  exit 1
fi

echo "Container smoke test passed: Compose config, image build, non-root read-only runtime, health/readiness, public proxy routes, and log redaction."
