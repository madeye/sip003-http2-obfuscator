# HTTP/2 Obfuscator — SIP003 Plugin for Shadowsocks

Wraps Shadowsocks traffic in HTTP/2 frames to bypass protocol detection.

## Usage

```bash
# Build
go build -o http2-obfuscator .

# Server side
ssserver -s "0.0.0.0:443" -k password -m aes-256-gcm \
  --plugin ./http2-obfuscator \
  --plugin-opts "mode=server;host=www.cloudfront.net;path=/cdn-cgi/trace"

# Client side
sslocal -s "server_ip:443" -b "127.0.0.1:1080" -k password -m aes-256-gcm \
  --plugin ./http2-obfuscator
```

## Plugin Options

| Option | Default | Description |
|--------|---------|-------------|
| `mode` | `client` | `server` enables HTTP/2 server mode (required on ss-server side) |
| `host` | `d1x3x9x7x5x3x9.cloudfront.net` | HTTP/2 `:authority` pseudo-header |
| `path` | `/cdn-cgi/trace` | HTTP/2 `:path` pseudo-header |
| `server_name` | `cloudfront.net` | TLS SNI hint (unused in TCP-only mode) |

## How It Works

The plugin implements [SIP003](https://shadowsocks.org/doc/sip003.html):

1. Shadowsocks spawns the plugin via environment variables (`SS_REMOTE_HOST`, `SS_LOCAL_HOST`, etc.)
2. Client-mode plugin wraps encrypted SS payload in HTTP/2 HEADERS + DATA frames
3. Server-mode plugin unwraves and forwards to ss-server
4. Response follows the reverse path

Traffic on the wire looks like standard HTTP/2 (connection preface, SETTINGS, HEADERS with `:method: POST`, `:path`, `:authority`, DATA frames).

## Test

```bash
bash test_integration.sh
```
