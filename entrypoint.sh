#!/bin/sh
set -e

# TUN device setup
mkdir -p /dev/net
if [ ! -c /dev/net/tun ]; then
    mknod /dev/net/tun c 10 200
    chmod 600 /dev/net/tun
fi

# Detect default gateway and interface for local network bypass
GW=$(ip route | awk '/default/ { print $3 }' | head -n 1)
ETH=$(ip route | awk '/default/ { print $5 }' | head -n 1)

if [ -n "$GW" ] && [ -n "$ETH" ]; then
    echo "[Entrypoint] Configuring local subnet bypass via $GW on $ETH..."
    ip rule add to 10.0.0.0/8 table 100 2>/dev/null || true
    ip rule add to 172.16.0.0/12 table 100 2>/dev/null || true
    ip rule add to 192.168.0.0/16 table 100 2>/dev/null || true
    ip rule add to 127.0.0.0/8 table 100 2>/dev/null || true
    ip route add default via "$GW" dev "$ETH" table 100 2>/dev/null || true
fi

exec /app/vpngate-proxy "$@"
