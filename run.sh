#!/data/data/com.termux/files/usr/bin/bash
# Isolated launcher: dedicated `opencode serve` (own XDG_DATA_HOME) + proxy.
# The interactive TUI keeps its own serve on 8377 / its own session store.
set -u
cd "$(dirname "$0")" || exit 1

PROXY_LISTEN=127.0.0.1:8099
PROXY_URL=http://127.0.0.1:8099
SERVE_URL=http://127.0.0.1:8399
DATA_HOME="${OPENCODE_PROXY_DATA_HOME:-$HOME/.local/share/opencode-proxy-svc}"
PIDFILE="$PWD/.run.pid"
SERVEPID="$PWD/.serve.pid"

alive() { [ -f "$1" ] && kill -0 "$(cat "$1")" 2>/dev/null; }

if [ ! -f config.json ]; then
  cat >config.json <<'JSON'
{
  "server": { "listen": "127.0.0.1:8099" },
  "providers": [
    {
      "name": "zen",
      "base_url": "https://opencode.ai/inference/openai/v1",
      "api_key": "public",
      "key_required": false,
      "mimic_opencode": true,
      "models": ["big-pickle"],
      "default_model": "big-pickle",
      "rps": 3,
      "timeout_ms": 180000
    },
    {
      "name": "serve",
      "kind": "serve",
      "base_url": "http://127.0.0.1:8399",
      "key_required": false,
      "agent": "general",
      "models": ["big-pickle"],
      "default_model": "big-pickle",
      "rps": 1,
      "timeout_ms": 300000
    }
  ],
  "aliases": {
    "pickle": { "provider": "zen", "model": "big-pickle" },
    "big-pickle": { "provider": "zen", "model": "big-pickle" },
    "free": { "provider": "zen", "model": "big-pickle" },
    "serve": { "provider": "serve", "model": "big-pickle" }
  },
  "default_provider": "zen"
}
JSON
  echo "created config.json (8099 -> zen direct + serve 8399 fallback)"
fi

mkdir -p "$DATA_HOME/opencode"
if [ ! -f "$DATA_HOME/opencode/auth.json" ] && [ -f "$HOME/.local/share/opencode/auth.json" ]; then
  cp "$HOME/.local/share/opencode/auth.json" "$DATA_HOME/opencode/auth.json"
  chmod 600 "$DATA_HOME/opencode/auth.json"
fi

if alive "$SERVEPID"; then
  echo "serve already up (pid $(cat "$SERVEPID"))"
else
  XDG_DATA_HOME="$DATA_HOME" nohup opencode serve --hostname 127.0.0.1 --port 8399 \
    >served.log 2>&1 &
  echo $! >"$SERVEPID"
  echo "serve started (pid $(cat "$SERVEPID")) on $SERVE_URL"
fi

for _ in $(seq 1 40); do
  curl -s -m 1 "$SERVE_URL/global/health" >/dev/null && break
  sleep 1
done
if ! curl -s -m 1 "$SERVE_URL/global/health" >/dev/null; then
  echo "serve never came up; check served.log" >&2
  exit 1
fi

if alive "$PIDFILE"; then
  echo "proxy already up (pid $(cat "$PIDFILE"))"
else
  if command -v go >/dev/null 2>&1; then
    go build -o ./opencode-api . || exit 1
  elif [ ! -x ./opencode-api ]; then
    echo "no go toolchain and no ./opencode-api binary; build it elsewhere or install go" >&2
    exit 1
  fi
  nohup ./opencode-api -config config.json >proxy.log 2>&1 &
  echo $! >"$PIDFILE"
  echo "proxy started (pid $(cat "$PIDFILE")) on $PROXY_URL"
fi

for _ in $(seq 1 30); do
  curl -s -m 1 "$PROXY_URL/v1/models" >/dev/null && break
  sleep 1
done
if curl -s -m 1 "$PROXY_URL/v1/models" >/dev/null; then
  echo "ready: $PROXY_URL/v1  (models: pickle, free, serve)"
else
  echo "proxy never answered; check proxy.log" >&2
  exit 1
fi
