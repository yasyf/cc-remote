#!/bin/sh

state="$HOME/.cc-remote"
if [ -x "$state/tailscaled.sh" ]; then
  nohup setsid "$state/tailscaled.sh" >> "$state/tailscaled.log" 2>&1 < /dev/null &
fi
if [ -x "$state/start.sh" ]; then
  exec "$state/start.sh"
fi
