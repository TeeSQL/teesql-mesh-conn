#!/bin/sh
set -eu

ip route add 127.77.0.0/16 dev lo 2>/dev/null || true

exec /usr/local/bin/teesql-mesh-conn "$@"
