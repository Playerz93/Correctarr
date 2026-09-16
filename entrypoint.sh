#!/bin/sh
# Drop to PUID:PGID (default nobody:users, Unraid's 99:100) after making sure
# the config directory is writable by that user.
set -e
PUID="${PUID:-99}"
PGID="${PGID:-100}"
DATA="${CORRECTARR_DATA:-/config}"

if [ "$(id -u)" = "0" ]; then
    mkdir -p "$DATA"
    if [ "$PUID" != "0" ]; then
        chown -R "$PUID:$PGID" "$DATA" 2>/dev/null || true
        exec su-exec "$PUID:$PGID" /usr/local/bin/correctarr "$@"
    fi
fi
exec /usr/local/bin/correctarr "$@"
