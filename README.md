# vpngate-proxy

A lightweight container that automatically connects to random Japan (JP) OpenVPN servers from [VPNGate.net](https://www.vpngate.net/) and exposes an HTTP/HTTPS forward proxy alongside a control API.

## Features

- **Zero Configuration**: Fetches and parses live VPNGate server configs on startup. No accounts or manual .ovpn setup required.
- **Ultra Lightweight**: Single Go binary in Alpine Linux (~30MB total Docker image).
- **Japan-only Filter**: Exclusively selects fast, low-latency Japanese VPN endpoints.
- **24-Hour Cooldown**: Prevents reconnecting to the same server within 24 hours of disconnection. Automatically persisted across restarts.
- **Failover / Rotation**: Instant server rotation via REST API (`GET /rotate`) or automatic fallback on connection failure.

## Ports

| Port | Service | Description |
| :--- | :--- | :--- |
| **1080** | HTTP/HTTPS Proxy | Forward proxy (supports CONNECT method for HTTPS) |
| **8080** | REST API | Management and monitoring API |

## Quick Start

### 1. Run with Docker Compose

```bash
git clone https://github.com/5seg/vpngate-proxy.git
cd vpngate-proxy
docker compose up -d
```

### 2. Verify Status

```bash
curl http://localhost:8080/status
```

Response:
```json
{
  "status": "connected",
  "current_server": {
    "hostname": "vpn999999999.opengw.net",
    "ip": "219.100.xx.xx",
    "ping": 18,
    "speed": 54200000,
    "country_short": "JP"
  },
  "public_ip": "219.100.xx.xx",
  "pool": {
    "total_jp_servers": 82,
    "blacklisted_last_24h": 0,
    "available_candidates": 82
  }
}
```

### 3. Use the Proxy

```bash
# Verify exit IP through the proxy
curl -x http://localhost:1080 http://ifconfig.me/ip
```

### 4. Rotate Server

```bash
curl -X GET http://localhost:8080/rotate
```
Returns `204 No Content` upon successful switch to a new random Japanese server.

## API Reference

- `GET /status` (HTTP 200): Returns current VPN state, connected server details, outbound IP, and candidate pool metrics.
- `GET /rotate` (HTTP 204): Disconnects the current server, marks it for 24-hour cooldown, and connects to another server. Returns `409` if rotation is already in progress, or `503` if all JP servers are in cooldown.

## Requirements

- Docker & Docker Compose
- Linux host or Docker environment supporting `NET_ADMIN` and `/dev/net/tun`

## License

MIT
