#!/bin/sh
set -eu

echo "Configuring transparent socket routing..."

ip rule add fwmark 1 lookup 100 2>/dev/null || true
ip route add local 0.0.0.0/0 dev lo table 100 2>/dev/null || true

nft delete table inet proxy2trans 2>/dev/null || true

nft add table inet proxy2trans
nft 'add chain inet proxy2trans divert { type filter hook prerouting priority mangle; policy accept; }'

nft add rule inet proxy2trans divert \
    meta l4proto tcp \
    socket transparent 1 \
    counter \
    meta mark set 1 \
    accept

nft add rule inet proxy2trans divert \
    meta l4proto udp \
    socket transparent 1 \
    counter \
    meta mark set 1 \
    accept

echo "Transparent routing configured."

exec /usr/local/bin/proxy2trans "$@"
