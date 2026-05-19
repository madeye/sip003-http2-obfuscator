#!/usr/bin/env bash
# Integration test for http2-obfuscator SIP003 plugin
# Tests: SOCKS5 → ss-local → plugin(client) ↔ plugin(server) → ss-server → target
set -euo pipefail

PLUGIN="${PWD}/http2-obfuscator"
SS_PORT=19000
SS_LOCAL_PORT=1080
SS_PASSWORD="test-pass-123"
SS_METHOD="aes-256-gcm"
TARGET_PORT=19090
TMPDIR=$(mktemp -d /tmp/http2obf_test.XXXXXX)
TARGET_PID=""
SSSERVER_PID=""
SSLOCAL_PID=""

cleanup() {
    echo "=== Cleanup ==="
    kill $TARGET_PID $SSSERVER_PID $SSLOCAL_PID 2>/dev/null || true
    wait $TARGET_PID $SSSERVER_PID $SSLOCAL_PID 2>/dev/null || true
    rm -rf "$TMPDIR"
    echo "=== Done ==="
}
trap cleanup EXIT INT TERM

echo "============================================"
echo " HTTP/2 Obfuscator SIP003 Plugin Test"
echo "============================================"

# Check prerequisites
for cmd in ssserver sslocal curl python3; do
    if ! command -v $cmd &>/dev/null; then
        echo "SKIP: $cmd not found"
        exit 0
    fi
done
if [ ! -f "$PLUGIN" ]; then
    echo "ERROR: plugin binary not found at $PLUGIN"
    exit 1
fi

# Start target HTTP server (Python)
echo "=== Starting target HTTP server on :$TARGET_PORT ==="
python3 -c "
import socket
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(('127.0.0.1', $TARGET_PORT))
s.listen(5)
while True:
    c, a = s.accept()
    c.sendall(b'HTTP/1.1 200 OK\r\nContent-Length: 13\r\nConnection: close\r\n\r\nHello, World!')
    c.close()
" &
TARGET_PID=$!
sleep 0.5

echo -n "  Target test: "
if curl -s --max-time 3 http://127.0.0.1:$TARGET_PORT/ | grep -q "Hello"; then
    echo "OK"
else
    echo "FAILED"
    exit 1
fi

# Start ss-server with plugin (server mode)
echo "=== Starting ss-server on :$SS_PORT (server mode) ==="
ssserver \
    -s "127.0.0.1:$SS_PORT" \
    -k "$SS_PASSWORD" \
    -m "$SS_METHOD" \
    --plugin "$PLUGIN" \
    --plugin-opts "mode=server;host=d1x3x9x7x5x3x9.cloudfront.net;path=/cdn-cgi/trace" \
    -v 2>"$TMPDIR/ssserver.log" &
SSSERVER_PID=$!
sleep 2

if ! kill -0 $SSSERVER_PID 2>/dev/null; then
    echo "  ss-server FAILED. Log:"
    cat "$TMPDIR/ssserver.log"
    exit 1
fi
echo "  ss-server PID: $SSSERVER_PID"

# Start ss-local with plugin (client mode)
echo "=== Starting ss-local (SOCKS5 :$SS_LOCAL_PORT, client mode) ==="
sslocal \
    -s "127.0.0.1:$SS_PORT" \
    -b "127.0.0.1:$SS_LOCAL_PORT" \
    -k "$SS_PASSWORD" \
    -m "$SS_METHOD" \
    --plugin "$PLUGIN" \
    -v 2>"$TMPDIR/sslocal.log" &
SSLOCAL_PID=$!
sleep 2

if ! kill -0 $SSLOCAL_PID 2>/dev/null; then
    echo "  ss-local FAILED. Log:"
    cat "$TMPDIR/sslocal.log"
    exit 1
fi
echo "  ss-local PID: $SSLOCAL_PID"

# Test via SOCKS5 proxy
echo ""
echo "=== Testing via SOCKS5 proxy ==="
echo "  curl --socks5-hostname 127.0.0.1:$SS_LOCAL_PORT http://127.0.0.1:$TARGET_PORT/"
echo "  (SS traffic flows through HTTP/2 obfuscation)"

RESULT=$(curl \
    --socks5-hostname 127.0.0.1:$SS_LOCAL_PORT \
    --max-time 15 \
    -s \
    http://127.0.0.1:$TARGET_PORT/ 2>&1 || true)

echo "  Response: '$RESULT'"

if echo "$RESULT" | grep -q "Hello, World!"; then
    echo ""
    echo "  ╔═══════════════════════════════════════╗"
    echo "  ║         TEST PASSED!                  ║"
    echo "  ║  HTTP/2 obfuscation works end-to-end  ║"
    echo "  ╚═══════════════════════════════════════╝"
else
    echo ""
    echo "  ╔════════════════════════════════╗"
    echo "  ║         TEST FAILED            ║"
    echo "  ╚════════════════════════════════╝"
    echo "  Expected: 'Hello, World!'"
    echo "  Got: '$RESULT'"
    echo ""
    echo "=== ss-server log ==="
    cat "$TMPDIR/ssserver.log"
    echo ""
    echo "=== ss-local log ==="
    cat "$TMPDIR/sslocal.log"
    exit 1
fi
