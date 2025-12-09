# Whelm Voice Server

High-performance voice relay and instant messaging server for the Whelm/Aerochat platform.

## Features

- **Voice Calling**: 48kbps Opus audio relay (96kbps both ways)
- **Instant Messaging**: Real-time message routing with MariaDB persistence
- **Live Status**: Heartbeat-based online/offline status tracking
- **Bandwidth Limiting**: Configurable 100Mbps up/down limits
- **Capacity Planning**: ~400 simultaneous calls max

## Resource Requirements

Based on 6 vCPU, 12GB RAM, 275mbps down / 225mbps up Debian server:

| Resource | Allocation | Notes |
|----------|-----------|-------|
| CPU | 1-2 vCPU | Go is efficient, mostly I/O bound |
| RAM | ~100MB | Minimal memory footprint |
| Storage | 10MB binary | + logs if enabled |
| Bandwidth | 100/100 Mbps | Self-limited, configurable |

### Max Capacity Calculation

```
Audio bitrate: 48 kbps per direction
Per call (server relay): 96kbps in + 96kbps out = 192 kbps
Bandwidth limit: 100 Mbps = 100,000 kbps
Max theoretical calls: 100,000 / 192 ≈ 520 calls
With 20% safety margin: ~400 simultaneous calls
```

## Installation (Debian)

### Prerequisites

```bash
sudo apt update
sudo apt install golang-go mariadb-client
```

### Build from source

```bash
git clone https://github.com/YOUR_USER/voiceMt.git
cd voiceMt
go build -o whelm-voice-server .
```

### Or download from GitHub Releases

```bash
wget https://github.com/YOUR_USER/voiceMt/releases/latest/download/whelm-voice-server-linux-amd64
chmod +x whelm-voice-server-linux-amd64
mv whelm-voice-server-linux-amd64 /usr/local/bin/whelm-voice-server
```

## Configuration

Set environment variables before running:

```bash
export WHELM_DB_DSN="whelm:yourpassword@tcp(db.example.com:3306)/whelm?parseTime=true"
```

## Running

### Direct

```bash
./whelm-voice-server
```

### As systemd service

```bash
sudo cp whelm-voice.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable whelm-voice
sudo systemctl start whelm-voice
```

## Protocol

### TCP Signaling (Port 5000)

Commands (newline-terminated):

| Command | Description |
|---------|-------------|
| `AUTH <token>` | Authenticate with session token |
| `HEARTBEAT [status]` | Keep-alive, optionally update status |
| `CALL <user_id>` | Initiate call to user |
| `ACCEPT <caller_id>` | Accept incoming call |
| `REJECT <caller_id>` | Reject incoming call |
| `HANGUP` | End current call |
| `MSG <recipient_id> <content>` | Send instant message |

Server responses:

| Response | Description |
|----------|-------------|
| `OK AUTH <user_id> <username>` | Auth successful |
| `OK HEARTBEAT` | Heartbeat acknowledged |
| `OK CALLING <user_id>` | Call request sent |
| `INCOMING <caller_id> <username>` | Incoming call notification |
| `START_CALL <partner_id> <username>` | Call connected |
| `REJECTED <user_id>` | Call was rejected |
| `HANGUP <user_id>` | Call ended by other party |
| `MSG <json>` | Incoming message |
| `ERROR <message>` | Error occurred |

### UDP Voice Relay (Port 5001)

1. Register with: `HELLO <user_id>`
2. Send Opus packets directly (server relays to call partner)

## Database Schema

Uses existing Whelm `users` and `messages` tables:

```sql
-- users table must have: id, username, session_token, status, last_seen
-- messages table must have: id, sender_id, channel_id, content, timestamp
```

## License

Same as Aerochat project.
