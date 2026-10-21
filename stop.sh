#!/data/data/com.termux/files/usr/bin/bash
# Stops only the processes this repo started. Never touches the TUI.
set -u
cd "$(dirname "$0")" || exit 1

for f in .run.pid .serve.pid; do
  if [ -f "$f" ]; then
    pid=$(cat "$f")
    if kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null && echo "killed $(basename "$f" .pid) pid $pid"
    fi
    rm -f "$f"
  fi
done
