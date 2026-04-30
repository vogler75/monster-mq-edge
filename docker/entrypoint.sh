#!/bin/sh
set -eu

if [ -f /config/config.yaml ]; then
    CONFIG=/config/config.yaml
else
    CONFIG=/etc/monstermq/config.yaml
fi

exec /monstermq-edge -config "$CONFIG" "$@"
