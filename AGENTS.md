# VPNGate HTTP Proxy Container

A lightweight Docker container that connects to random Japan (JP) OpenVPN servers provided by VPNGate.net, providing an HTTP/HTTPS forward proxy and a control REST API.

## Key Features

1. **Lightweight Architecture**:
   - Single statically-compiled Go binary running inside Alpine Linux.
   - Built-in HTTP/HTTPS CONNECT forward proxy and REST API server. No heavy runtime or separate proxy daemon needed.
   - Image footprint is minimal (~25-35 MB total).

2. **Server Filtering & Random Selection**:
   - Fetches the dynamic list of public servers directly from `http://www.vpngate.net/api/iphone/`.
   - Filters servers exclusively to region Japan (`CountryShort == "JP"`).
   - Randomly picks a candidate server for connection.

3. **24-Hour Cooldown (Anti-Reconnection)**:
   - Once a server is disconnected (or fails to connect), its IP address is recorded into a cooldown ledger (`/data/history.json`).
   - The server will **not** be reconnected to until 24 hours have elapsed since disconnection.
   - Cooldown data is persisted across container restarts via the `/data` volume.

4. **Exhaustion Error Handling**:
   - If all available JP servers are within their 24-hour cooldown period, the container flags an error state (`exhausted`).
   - `/rotate` calls will return `503 Service Unavailable` when no servers are available.

5. **Connecting State Handling**:
   - Proxy requests arriving while a VPN handshake is in progress return `503 Service Unavailable` with `X-Proxy-Status: connecting`.
   - `/rotate` requests return `409 Conflict` if a rotation is already ongoing.
   - Connection timeout per server attempt is 15 seconds; upon timeout or error, it automatically falls back to another JP server.

---

## Network Ports

| Port | Protocol | Purpose |
| :--- | :--- | :--- |
| **1080** | HTTP / HTTPS (CONNECT) | Forward Proxy for client traffic |
| **8080** | HTTP (REST) | Control & Status API |

---

## API Endpoints

### 1. `GET /status`
Returns the current health, server details, public IP, and pool metrics.

- **Response Code**: `200 OK`
- **Response Format**: `application/json`

**Example Response**:
```json
{
  "status": "connected",
  "current_server": {
    "hostname": "vpn999999999.opengw.net",
    "ip": "219.100.xx.xx",
    "score": 150240,
    "ping": 18,
    "speed": 54200000,
    "country_long": "Japan",
    "country_short": "JP",
    "connected_at": "2026-09-23T00:30:00Z"
  },
  "public_ip": "219.100.xx.xx",
  "pool": {
    "total_jp_servers": 82,
    "blacklisted_last_24h": 5,
    "available_candidates": 77
  }
}
```

Status values:
- `"connecting"`: Initial connection in progress.
- `"connected"`: Active VPN tunnel established and proxy ready.
- `"rotating"`: Disconnecting current VPN and switching to another server.
- `"exhausted"`: No JP servers available (all in 24h cooldown).
- `"error"`: Fatal error encountered.

### 2. `GET /rotate`
Triggers an immediate switch to another random JP VPN server.

- **Success**: `204 No Content` (blocks until the new connection is ready).
- **Conflict**: `409 Conflict` (rotation already in progress).
- **Exhaustion**: `503 Service Unavailable` (no servers available outside 24h cooldown).

---

## Running the Container

### Prerequisites
The container requires Linux network capabilities to configure the OpenVPN tunnel device (`tun0`):
- `cap_add: [NET_ADMIN]`
- `devices: [/dev/net/tun:/dev/net/tun]`

### Via Docker Compose
```bash
cd /home/master/vpngate-proxy
docker compose up -d
```

### Usage Examples
```bash
# Check status
curl http://localhost:8080/status

# Rotate to a new VPN server
curl -X GET http://localhost:8080/rotate

# Use the proxy
curl -x http://localhost:1080 http://ifconfig.me/ip
```
