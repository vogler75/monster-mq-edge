#!/bin/sh
set -eu

if [ -f /cfg-data/config.yaml ]; then
    CONFIG=/cfg-data/config.yaml
else
    CONFIG=/etc/monstermq/config.yaml
fi

exec /monstermq-edge -config "$CONFIG" "$@"
