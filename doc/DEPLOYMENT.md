# Operations and Deployment Guide

**Version**: 2.0
**Date**: 2026-02-09
**Status**: Draft

This document covers deployment, operations, and production configuration for nanosec.

> **过时提示（2026-10-18）**：本文档继承自上游项目，描述「单镜像 + systemd」的部署形态
> （Telegram/Discord/WhatsApp 桥、`~/.nanosec/*.json` 配置）。平台的权威部署指南是
> 超级仓库的 `docs/DEPLOYMENT.md`。存储已从 MySQL 迁到 **MongoDB**（由 `nanobus` 独占，
> 其他服务经总线访问）：下文的连接方式、备份脚本与 systemd 依赖都已按此更新，但整体
> 部署形态仍属历史描述。

---

## 0. Architecture Overview

Nanosec uses a **three-process architecture**:

| Process | Binary | Purpose | Scaling |
|---------|--------|---------|---------|
| **Bus** | `nanobus` | User registry, message queues (MongoDB) | Multiple behind LB |
| **Gateway** | `nanogateway` | Platform adapters, user resolution | Multiple per channel |
| **Agent** | `nanosec --user=X` | LLM calls, tools, sessions | One per user |

```
┌─────────────┐     ┌─────────────┐     ┌─────────────┐
│   Gateway   │────▶│     Bus     │◀────│   Agent     │
│  (Telegram) │     │   (Mongo)   │     │  (alice)    │
└─────────────┘     └─────────────┘     └─────────────┘
                           │
                    ┌──────┴──────┐
                    │   Agent     │
                    │   (bob)     │
                    └─────────────┘
```

---

## 1. Binary Distribution

### 1.1 Building from Source

```bash
# Clone repository
git clone https://github.com/user/nanosec.git
cd nanosec

# Build all binaries for current platform
make build

# Or build individually
go build -o nanobus ./cmd/nanobus
go build -o nanogateway ./cmd/nanogateway
go build -o nanosec ./cmd/nanosec

# Build with version info
make build-release VERSION=v0.1.0

# Build optimized binaries
go build -ldflags "-s -w" -o nanobus ./cmd/nanobus
go build -ldflags "-s -w" -o nanogateway ./cmd/nanogateway
go build -ldflags "-s -w" -o nanosec ./cmd/nanosec
```

### 1.2 Cross-Compilation

```bash
# Build all binaries for a target platform:

# Linux AMD64
GOOS=linux GOARCH=amd64 go build -o nanobus-linux-amd64 ./cmd/nanobus
GOOS=linux GOARCH=amd64 go build -o nanogateway-linux-amd64 ./cmd/nanogateway
GOOS=linux GOARCH=amd64 go build -o nanosec-linux-amd64 ./cmd/nanosec

# Linux ARM64 (Raspberry Pi, etc.)
GOOS=linux GOARCH=arm64 go build -o nanobus-linux-arm64 ./cmd/nanobus
GOOS=linux GOARCH=arm64 go build -o nanogateway-linux-arm64 ./cmd/nanogateway
GOOS=linux GOARCH=arm64 go build -o nanosec-linux-arm64 ./cmd/nanosec

# macOS ARM64 (Apple Silicon)
GOOS=darwin GOARCH=arm64 go build -o nanobus-darwin-arm64 ./cmd/nanobus
GOOS=darwin GOARCH=arm64 go build -o nanogateway-darwin-arm64 ./cmd/nanogateway
GOOS=darwin GOARCH=arm64 go build -o nanosec-darwin-arm64 ./cmd/nanosec
```

### 1.3 goreleaser Configuration

```yaml
# .goreleaser.yaml
version: 1

project_name: nanosec

before:
  hooks:
    - go mod tidy

builds:
  - id: nanobus
    main: ./cmd/nanobus
    binary: nanobus
    env:
      - CGO_ENABLED=0
    goos:
      - linux
      - darwin
    goarch:
      - amd64
      - arm64
    ldflags:
      - -s -w
      - -X main.version={{.Version}}
      - -X main.commit={{.Commit}}
      - -X main.date={{.Date}}

  - id: nanogateway
    main: ./cmd/nanogateway
    binary: nanogateway
    env:
      - CGO_ENABLED=0
    goos:
      - linux
      - darwin
    goarch:
      - amd64
      - arm64
    ldflags:
      - -s -w
      - -X main.version={{.Version}}
      - -X main.commit={{.Commit}}
      - -X main.date={{.Date}}

  - id: nanosec
    main: ./cmd/nanosec
    binary: nanosec
    env:
      - CGO_ENABLED=0
    goos:
      - linux
      - darwin
    goarch:
      - amd64
      - arm64
    ldflags:
      - -s -w
      - -X main.version={{.Version}}
      - -X main.commit={{.Commit}}
      - -X main.date={{.Date}}

archives:
  - id: default
    format: tar.gz
    format_overrides:
      - goos: windows
        format: zip
    name_template: "{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}"
    files:
      - LICENSE
      - README.md
      - configs/config.example.json

checksum:
  name_template: 'checksums.txt'

changelog:
  sort: asc
  filters:
    exclude:
      - '^docs:'
      - '^test:'
      - '^ci:'

release:
  github:
    owner: user
    name: nanosec
  draft: false
  prerelease: auto
```

### 1.4 Release Process

```bash
# Tag a release
git tag -a v0.1.0 -m "Release v0.1.0"
git push origin v0.1.0

# Build release with goreleaser
goreleaser release --clean

# Dry run (test without publishing)
goreleaser release --snapshot --clean
```

---

## 2. Docker Deployment

### 2.1 Dockerfile

```dockerfile
# Dockerfile
FROM golang:1.22-alpine AS builder

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY . .

# Build all binaries
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /nanobus ./cmd/nanobus
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /nanogateway ./cmd/nanogateway
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /nanosec ./cmd/nanosec

# Runtime image
FROM ubuntu:24.04

RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates tzdata bash curl tar \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Copy binaries
COPY --from=builder /nanobus /usr/local/bin/nanobus
COPY --from=builder /nanogateway /usr/local/bin/nanogateway
COPY --from=builder /nanosec /usr/local/bin/nanosec

# Create nanosec user (the per-user name is created at container start by the
# entrypoint; for a fixed single-service image the default 'ubuntu' user at
# uid 1000 is renamed to the runtime identity)
RUN usermod -l nanosec -d /home/nanosec -m -s /bin/bash ubuntu \
    && groupmod -n nanosec ubuntu \
    && chown -R nanosec:nanosec /home/nanosec
USER nanosec

# Default command (override per service)
ENTRYPOINT ["nanobus"]
```

### 2.2 Docker Compose

```yaml
# docker-compose.yml
version: '3.8'

services:
  # MongoDB is external: it is not part of this stack, and the bus is the only
  # service that connects to it (NANOSEC_BUS_DATABASE_URI below). See the
  # platform's docs/DEPLOYMENT.md for the supported stack.

  bus:
    build: .
    image: nanosec:latest
    container_name: nanosec-bus
    restart: unless-stopped
    volumes:
      - ./configs/bus.json:/home/nanosec/.nanosec/bus.json:ro
    environment:
      - TZ=UTC
      - NANOSEC_BUS_DATABASE_URI=mongodb://nanosec:${MONGO_PASSWORD}@mongodb.example.com:27017/nanobus?authSource=admin
    ports:
      - "9000:9000"
    command: ["nanobus", "--config", "/home/nanosec/.nanosec/bus.json"]

  gateway:
    build: .
    image: nanosec:latest
    container_name: nanosec-gateway
    restart: unless-stopped
    depends_on:
      - bus
    volumes:
      - ./configs/gateway.json:/home/nanosec/.nanosec/gateway.json:ro
    environment:
      - TZ=UTC
      - NANOSEC_GATEWAY_BUS_URL=ws://bus:9000
    command: ["nanogateway", "--config", "/home/nanosec/.nanosec/gateway.json"]

  # Agent for user "alice"
  agent-alice:
    build: .
    image: nanosec:latest
    container_name: nanosec-agent-alice
    restart: unless-stopped
    depends_on:
      - bus
    volumes:
      - ./configs/agent.json:/home/nanosec/.nanosec/agent.json:ro
      - agent-alice-data:/home/nanosec/.nanosec
    environment:
      - TZ=UTC
      - NANOSEC_AGENT_BUS_URL=ws://bus:9000
    command: ["nanosec", "--user=alice", "--config", "/home/nanosec/.nanosec/agent.json"]

  # Optional: WhatsApp bridge
  whatsapp-bridge:
    image: node:20-alpine
    container_name: whatsapp-bridge
    restart: unless-stopped
    working_dir: /app
    volumes:
      - ./bridge:/app
      - whatsapp-auth:/app/auth
    command: npm start
    ports:
      - "3001:3001"

volumes:
  agent-alice-data:
  whatsapp-auth:
```

### 2.3 Docker Commands

```bash
# Build image
docker build -t nanosec:latest .

# Run bus (points at your external MongoDB; the bus owns that database)
docker run -d \
  --name nanosec-bus \
  -p 9000:9000 \
  -e NANOSEC_BUS_DATABASE_URI="mongodb://nanosec:secret@db.example.com:27017/nanobus?authSource=admin" \
  nanosec:latest nanobus

# Run gateway
docker run -d \
  --name nanosec-gateway \
  --link nanosec-bus:bus \
  -v $(pwd)/gateway.json:/home/nanosec/.nanosec/gateway.json:ro \
  -e NANOSEC_GATEWAY_BUS_URL=ws://bus:9000 \
  --entrypoint nanogateway \
  nanosec:latest

# Run agent for alice
docker run -d \
  --name nanosec-agent-alice \
  --link nanosec-bus:bus \
  -v $(pwd)/agent.json:/home/nanosec/.nanosec/agent.json:ro \
  -v agent-alice-data:/home/nanosec/.nanosec \
  -e NANOSEC_AGENT_BUS_URL=ws://bus:9000 \
  --entrypoint nanosec \
  nanosec:latest --user=alice

# View logs
docker logs -f nanosec-bus
docker logs -f nanosec-gateway
docker logs -f nanosec-agent-alice

# With docker-compose (recommended)
docker-compose up -d
docker-compose logs -f
docker-compose down
```

---

## 3. Configuration Management

### 3.1 Configuration File Locations

Each service has its own configuration file:

| Service | Default Path |
|---------|--------------|
| Bus | `~/.nanosec/bus.json` |
| Gateway | `~/.nanosec/gateway.json` |
| Agent | `~/.nanosec/agent.json` |

| Platform | Config Directory |
|----------|-----------------|
| Linux | `~/.nanosec/` |
| macOS | `~/.nanosec/` |
| Windows | `%USERPROFILE%\.nanosec\` |
| Docker | `/home/nanosec/.nanosec/` |

### 3.2 Bus Configuration (`bus.json`)

```json
{
  "server": {
    "port": 9000,
    "host": "0.0.0.0"
  },
  "database": {
    "uri": "mongodb://user:password@localhost:27017/nanobus?authSource=admin"
  }
}
```

### 3.3 Gateway Configuration (`gateway.json`)

```json
{
  "bus": {
    "url": "ws://localhost:9000"
  },
  "channels": {
    "cli": {
      "enabled": true,
      "userID": "alice"
    },
    "wecom": {
      "enabled": false,
      "corpID": "",
      "corpSecret": "",
      "agentID": 0,
      "token": "",
      "encodingAESKey": ""
    },
    "feishu": {
      "enabled": false,
      "appId": "",
      "appSecret": ""
    }
  }
}
```

### 3.4 Agent Configuration (`agent.json`)

```json
{
  "bus": {
    "url": "ws://localhost:9000"
  },
  "provider": {
    "apiKey": "sk-...",
    "baseUrl": "https://api.openai.com/v1",
    "defaultModel": "gpt-4o",
    "subagentModel": "gpt-4o-mini"
  },
  "agent": {
    "maxIterations": 20
  },
  "tools": {
    "restrictToWorkspace": true,
    "shell": {
      "timeout": "60s"
    },
    "web": {
      "search": {
        "apiKey": "",
        "maxResults": 5
      }
    }
  },
  "paths": {
    "dataRoot": "~/.nanosec",
    "sessionDir": "~/.nanosec/sessions",
    "cronFile": "~/.nanosec/cron.json",
    "mediaBaseDir": "~/.nanosec/media",
    "workspaceDir": "/workspace",
    "identityFile": "/workspace/AGENT.md",
    "bootstrapDir": "/workspace/bootstrap",
    "memoryDir": "/workspace/memory",
    "skillsDir": "/workspace/skills"
  }
}
```

### 3.5 Environment Variable Overrides

```bash
# Bus settings
export NANOSEC_BUS_SERVER_PORT=9000
export NANOSEC_BUS_DATABASE_URI="mongodb://user:pass@localhost:27017/nanobus?authSource=admin"

# Gateway settings
export NANOSEC_GATEWAY_BUS_URL="ws://localhost:9000"
export NANOSEC_GATEWAY_CHANNELS_WECOM_CORPID="your-corp-id"

# Agent settings
export NANOSEC_AGENT_BUS_URL="ws://localhost:9000"
export NANOSEC_AGENT_PROVIDER_APIKEY="sk-..."
export NANOSEC_AGENT_PROVIDER_BASEURL="https://api.openai.com/v1"
export NANOSEC_AGENT_PROVIDER_DEFAULTMODEL="gpt-4o"
export NANOSEC_AGENT_AGENT_MAXITERATIONS="30"
```

### 3.6 Secrets Management

**DO NOT** commit secrets to version control. Options:

1. **Environment variables**: Set in shell or container
2. **Docker secrets**: Use Docker Swarm secrets
3. **External secrets manager**: HashiCorp Vault, AWS Secrets Manager
4. **Encrypted config**: Use SOPS or age for encryption

```bash
# Using SOPS for encrypted config
sops -e config.json > config.enc.json
sops -d config.enc.json > config.json
```

---

## 4. Systemd Services

### 4.1 Bus Service File

```ini
# /etc/systemd/system/nanosec-bus.service
[Unit]
Description=Nanosec Bus Service
After=network.target
Wants=network-online.target

[Service]
Type=simple
User=nanosec
Group=nanosec
WorkingDirectory=/home/nanosec
ExecStart=/usr/local/bin/nanobus --config /home/nanosec/.nanosec/bus.json
Restart=on-failure
RestartSec=10

# Security hardening
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=/home/nanosec/.nanosec

Environment=HOME=/home/nanosec

[Install]
WantedBy=multi-user.target
```

### 4.2 Gateway Service File

```ini
# /etc/systemd/system/nanosec-gateway.service
[Unit]
Description=Nanosec Gateway Service
After=network.target nanosec-bus.service
Requires=nanosec-bus.service

[Service]
Type=simple
User=nanosec
Group=nanosec
WorkingDirectory=/home/nanosec
ExecStart=/usr/local/bin/nanogateway --config /home/nanosec/.nanosec/gateway.json
Restart=on-failure
RestartSec=10

NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=read-only

Environment=HOME=/home/nanosec

[Install]
WantedBy=multi-user.target
```

### 4.3 Agent Service Template

```ini
# /etc/systemd/system/nanosec-agent@.service
[Unit]
Description=Nanosec Agent for %i
After=network.target nanosec-bus.service
Requires=nanosec-bus.service

[Service]
Type=simple
User=nanosec
Group=nanosec
WorkingDirectory=/home/nanosec
ExecStart=/usr/local/bin/nanosec --user=%i --config /home/nanosec/.nanosec/agent.json
Restart=on-failure
RestartSec=10

NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=read-only
ReadWritePaths=/home/nanosec/.nanosec

Environment=HOME=/home/nanosec

[Install]
WantedBy=multi-user.target
```

### 4.4 Installation

```bash
# Copy binaries
sudo cp nanobus nanogateway nanosec /usr/local/bin/
sudo chmod +x /usr/local/bin/nanobus /usr/local/bin/nanogateway /usr/local/bin/nanosec

# Create user
sudo useradd -r -m -s /bin/false nanosec

# Create config directory
sudo -u nanosec mkdir -p /home/nanosec/.nanosec
sudo cp configs/*.json /home/nanosec/.nanosec/
sudo chown -R nanosec:nanosec /home/nanosec/.nanosec

# Install services
sudo cp nanosec-bus.service /etc/systemd/system/
sudo cp nanosec-gateway.service /etc/systemd/system/
sudo cp nanosec-agent@.service /etc/systemd/system/
sudo systemctl daemon-reload

# Enable and start services
sudo systemctl enable nanosec-bus nanosec-gateway
sudo systemctl start nanosec-bus nanosec-gateway

# Start agents for specific users
sudo systemctl enable nanosec-agent@alice
sudo systemctl start nanosec-agent@alice
sudo systemctl enable nanosec-agent@bob
sudo systemctl start nanosec-agent@bob
```

### 4.5 Management Commands

```bash
# Start/stop/restart all
sudo systemctl start nanosec-bus nanosec-gateway
sudo systemctl stop nanosec-bus nanosec-gateway
sudo systemctl restart nanosec-bus nanosec-gateway

# Manage specific agent
sudo systemctl start nanosec-agent@alice
sudo systemctl stop nanosec-agent@alice
sudo systemctl status nanosec-agent@alice

# View logs
sudo journalctl -u nanosec-bus -f
sudo journalctl -u nanosec-gateway -f
sudo journalctl -u nanosec-agent@alice -f

# View all nanosec logs
sudo journalctl -u 'nanosec-*' -f
```

---

## 5. Monitoring and Logging

### 5.1 Structured Logging

Nanosec uses Go's `log/slog` for structured logging:

```go
// Log levels
slog.Debug("message received", "channel", "telegram", "user", userID)
slog.Info("agent started", "model", model)
slog.Warn("rate limit approaching", "remaining", 10)
slog.Error("tool failed", "tool", name, "error", err)
```

### 5.2 Log Output

```bash
# Console output (default)
time=2026-02-08T10:00:00Z level=INFO msg="agent started" model=claude-3

# JSON output (for log aggregation)
{"time":"2026-02-08T10:00:00Z","level":"INFO","msg":"agent started","model":"claude-3"}
```

### 5.3 Log Aggregation

For production, forward logs to a centralized system:

```yaml
# docker-compose.yml with logging
services:
  bus:
    # ...
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"

  # Or use a log driver
  gateway:
    # ...
    logging:
      driver: syslog
      options:
        syslog-address: "udp://logs.example.com:514"
```

### 5.4 Health Checks

```yaml
# docker-compose.yml with health check
services:
  bus:
    # ...
    healthcheck:
      test: ["CMD", "nanobus", "version"]
      interval: 30s
      timeout: 10s
      retries: 3
      start_period: 10s
```

---

## 6. Backup and Recovery

### 6.1 Data to Back Up

| Data | Location | Description |
|------|----------|-------------|
| MongoDB | external MongoDB (`nanobus` database) | Users, channels, message queues |
| Bus Config | `~/.nanosec/bus.json` | Bus settings |
| Gateway Config | `~/.nanosec/gateway.json` | Channel credentials |
| Agent Config | `~/.nanosec/agent.json` | Provider API keys |
| Sessions | `~/.nanosec/sessions/` | Conversation history (per agent) |
| Cron jobs | `~/.nanosec/cron.json` | Scheduled tasks |
| Workspace | `~/.nanosec/workspace/` | Agent working files |

### 6.2 Backup Script

```bash
#!/bin/bash
# backup-nanosec.sh

BACKUP_DIR="/backups/nanosec"
DATE=$(date +%Y%m%d_%H%M%S)
NANOSEC_DIR="$HOME/.nanosec"
MONGO_URI="mongodb://nanosec:password@localhost:27017/nanobus?authSource=admin"

# Create backup directory
mkdir -p "$BACKUP_DIR"

# Backup the MongoDB the bus owns
mongodump --uri="$MONGO_URI" --archive="$BACKUP_DIR/db_$DATE.archive" --gzip

# Backup config and sessions
tar -czf "$BACKUP_DIR/files_$DATE.tar.gz" "$NANOSEC_DIR"

# Keep only last 7 backups
ls -t "$BACKUP_DIR"/db_*.archive | tail -n +8 | xargs -r rm
ls -t "$BACKUP_DIR"/files_*.tar.gz | tail -n +8 | xargs -r rm

echo "Backup created: $BACKUP_DIR/*_$DATE.*"
```

### 6.3 Restore

```bash
#!/bin/bash
# restore-nanosec.sh

DB_BACKUP="$1"
FILES_BACKUP="$2"

if [ -z "$DB_BACKUP" ] || [ -z "$FILES_BACKUP" ]; then
  echo "Usage: $0 <db_backup.archive> <files_backup.tar.gz>"
  exit 1
fi

# The archive to restore was produced by `mongodump --archive --gzip`
MONGO_URI="mongodb://nanosec:password@localhost:27017/nanobus?authSource=admin"

# Stop services
sudo systemctl stop nanosec-gateway
sudo systemctl stop 'nanosec-agent@*'
sudo systemctl stop nanosec-bus

# Restore database
mongorestore --uri="$MONGO_URI" --archive="$DB_BACKUP" --gzip

# Restore files
tar -xzf "$FILES_BACKUP" -C /

# Start services
sudo systemctl start nanosec-bus
sudo systemctl start nanosec-gateway
sudo systemctl start nanosec-agent@alice
```

---

## 7. Security Considerations

### 7.1 Production Checklist

- [ ] Enable workspace restriction (`restrictToWorkspace: true`)
- [ ] Configure user allowlists for all channels
- [ ] Use strong API keys
- [ ] Run as non-root user
- [ ] Use HTTPS for webhook endpoints (if applicable)
- [ ] Regularly rotate API keys
- [ ] Monitor for unusual activity
- [ ] Keep dependencies updated

### 7.2 Firewall Rules

```bash
# Only allow outbound connections (no inbound required for polling)
# Telegram: HTTPS to api.telegram.org
# Discord: WSS to gateway.discord.gg
# LLM APIs: HTTPS to various providers

# If running WhatsApp bridge locally
sudo ufw allow from 127.0.0.1 to 127.0.0.1 port 3001
```

### 7.3 API Key Rotation

```bash
# 1. Generate new API key from provider
# 2. Update config.json with new key
# 3. Restart service
sudo systemctl restart nanosec-bus nanosec-gateway
# 4. Revoke old key from provider
```

---

## 8. Troubleshooting

### 8.1 Common Issues

| Issue | Cause | Solution |
|-------|-------|----------|
| "connection refused" | Channel not configured | Check token/credentials |
| "rate limit exceeded" | Too many API calls | Implement backoff, check limits |
| "context deadline exceeded" | Timeout | Increase timeout in config |
| "permission denied" | Workspace restriction | Check paths, adjust workspace |
| "tool not found" | Tool not registered | Check tool configuration |

### 8.2 Debug Mode

```bash
# Run with debug logging
NANOSEC_LOG_LEVEL=debug nanogateway

# Or set in config
{
  "logging": {
    "level": "debug"
  }
}
```

### 8.3 Connection Testing

```bash
# Test Telegram connection
curl -s "https://api.telegram.org/bot<TOKEN>/getMe" | jq

# Test OpenRouter
curl -s "https://openrouter.ai/api/v1/models" \
  -H "Authorization: Bearer <API_KEY>" | jq

# Test Discord
# Use the gateway command and check logs
```

---

## 9. Performance Tuning

### 9.1 Resource Limits

```yaml
# docker-compose.yml
services:
  bus:
    # ...
    deploy:
      resources:
        limits:
          cpus: '1'
          memory: 256M
        reservations:
          cpus: '0.25'
          memory: 64M
```

### 9.2 Go Runtime Tuning

```bash
# Limit goroutine stack size (if memory constrained)
export GODEBUG=morestack=10

# Adjust garbage collection
export GOGC=100  # Default, lower = more frequent GC
```

### 9.3 Channel Buffer Sizes

For high-traffic deployments, adjust buffer sizes in config:

```json
{
  "bus": {
    "inboundBufferSize": 500,
    "outboundBufferSize": 500
  }
}
```

---

## 10. Upgrade Process

### 10.1 Standard Upgrade

```bash
# 1. Download new version
wget https://github.com/user/nanosec/releases/latest/download/nanosec_linux_amd64.tar.gz

# 2. Extract
tar -xzf nanosec_linux_amd64.tar.gz

# 3. Stop services (reverse order)
sudo systemctl stop 'nanosec-agent@*'
sudo systemctl stop nanosec-gateway
sudo systemctl stop nanosec-bus

# 4. Backup current binaries
sudo cp /usr/local/bin/nanobus /usr/local/bin/nanobus.bak
sudo cp /usr/local/bin/nanogateway /usr/local/bin/nanogateway.bak
sudo cp /usr/local/bin/nanosec /usr/local/bin/nanosec.bak

# 5. Install new binaries
sudo cp nanobus nanogateway nanosec /usr/local/bin/
sudo chmod +x /usr/local/bin/nanobus /usr/local/bin/nanogateway /usr/local/bin/nanosec

# 6. Run migrations if needed
nanobus migrate --config ~/.nanosec/bus.json

# 7. Start services
sudo systemctl start nanosec-bus
sudo systemctl start nanosec-gateway
sudo systemctl start nanosec-agent@alice

# 8. Verify
sudo systemctl status 'nanosec-*'
nanobus version
nanogateway version
nanosec version
```

### 10.2 Rollback

```bash
# 1. Stop services
sudo systemctl stop 'nanosec-agent@*'
sudo systemctl stop nanosec-gateway
sudo systemctl stop nanosec-bus

# 2. Restore backup
sudo cp /usr/local/bin/nanobus.bak /usr/local/bin/nanobus
sudo cp /usr/local/bin/nanogateway.bak /usr/local/bin/nanogateway
sudo cp /usr/local/bin/nanosec.bak /usr/local/bin/nanosec

# 3. Start services
sudo systemctl start nanosec-bus
sudo systemctl start nanosec-gateway
sudo systemctl start nanosec-agent@alice
```

---

## Revision History

| Version | Date | Author | Changes |
|---------|------|--------|---------|
| 1.0 | 2026-02-08 | Claude | Initial draft |
| 2.1 | 2026-02-22 | Claude | Updated gateway config examples (Telegram/Discord → WeCom/Feishu/CLI), removed orphaned restore script fragment |
