# Jenkins + Docker Deployment Guide

**Audience**: teams with an existing MySQL instance who want to build and deploy nanosec via a Jenkins CI/CD pipeline.

---

## Overview

The pipeline builds a single Docker image containing all three binaries, pushes it to your registry, then SSH-deploys to the target host using `docker compose`. MySQL is external — only the three nanosec containers are managed here.

```
┌─────────────┐   build/push   ┌──────────────────┐   SSH deploy   ┌────────────────────┐
│   Jenkins   │ ─────────────▶ │ Docker Registry  │ ─────────────▶ │   Deploy Host      │
│  (pipeline) │                └──────────────────┘                │  ┌──────────────┐  │
└─────────────┘                                                     │  │   nanobus    │  │
                                                                    │  │  nanogateway │  │
                                                                    │  │  nanosec     │  │
                                                                    │  └──────────────┘  │
                                                                    │  (existing MySQL)  │
                                                                    └────────────────────┘
```

---

## Prerequisites

- Jenkins with the following plugins:
  - **Docker Pipeline** (`docker.build`, `docker.withRegistry`)
  - **SSH Agent** (for deploying to the target host)
  - **Credentials Binding**
- A Docker registry (Docker Hub, Harbor, your own, or Jenkins' local daemon)
- The deploy host has **Docker Engine** and **docker compose v2** installed
- The deploy host runs **nginx** with a valid TLS certificate for the WeCom callback URL

---

## Step 1 — Files to Add to the Repository

### 1.1 `docker-compose.prod.yml`

This replaces `docker-compose.yml` for production. It assumes all secrets are injected as environment variables (set by Jenkins at deploy time).

```yaml
# docker-compose.prod.yml
version: "3.9"

services:
  nanobus:
    image: ${IMAGE}
    entrypoint: nanobus
    restart: unless-stopped
    command: ["--config", "/etc/nanosec/bus.json"]
    configs:
      - source: bus_config
        target: /etc/nanosec/bus.json
    networks:
      - nanosec
    healthcheck:
      test: ["CMD", "sh", "-c", "nc -z 127.0.0.1 9000"]
      interval: 10s
      timeout: 5s
      retries: 6
      start_period: 5s

  nanogateway:
    image: ${IMAGE}
    entrypoint: nanogateway
    restart: unless-stopped
    command: ["--config", "/etc/nanosec/gateway.json"]
    configs:
      - source: gateway_config
        target: /etc/nanosec/gateway.json
    ports:
      - "127.0.0.1:8081:8081"    # WeCom callback — nginx proxies here
    depends_on:
      nanobus:
        condition: service_healthy
    networks:
      - nanosec

  # One service per user. Add a block for each additional user.
  agent-alice:
    image: ${IMAGE}
    entrypoint: nanosec
    restart: unless-stopped
    command: ["--user", "alice", "--config", "/etc/nanosec/agent.json"]
    configs:
      - source: agent_config
        target: /etc/nanosec/agent.json
    volumes:
      - agent_alice_data:/workspace
    depends_on:
      nanobus:
        condition: service_healthy
    networks:
      - nanosec

configs:
  bus_config:
    file: ./configs/bus.json
  gateway_config:
    file: ./configs/gateway.json
  agent_config:
    file: ./configs/agent.json

volumes:
  agent_alice_data:

networks:
  nanosec:
    driver: bridge
```

> The bus port `9000` is **not** exposed to the host — all inter-service communication uses the `nanosec` Docker network, where services reach each other by container name.

### 1.2 `configs/bus.json`

```json
{
  "server": {
    "host": "0.0.0.0",
    "port": 9000
  },
  "database": {
    "driver": "mysql",
    "dsn": "__BUS_DB_DSN__"
  }
}
```

### 1.3 `configs/gateway.json`

```json
{
  "bus": {
    "url": "ws://nanobus:9000"
  },
  "channels": {
    "cli": { "enabled": false },
    "wecom": {
      "enabled": true,
      "corpID":         "__WECOM_CORP_ID__",
      "corpSecret":     "__WECOM_CORP_SECRET__",
      "agentID":        0,
      "token":          "__WECOM_TOKEN__",
      "encodingAESKey": "__WECOM_AES_KEY__",
      "callbackAddr":   "0.0.0.0:8081"
    },
    "feishu": { "enabled": false }
  }
}
```

### 1.4 `configs/agent.json`

```json
{
  "bus": {
    "url": "ws://nanobus:9000"
  },
  "provider": {
    "apiKey":       "__LLM_API_KEY__",
    "baseUrl":      "https://api.openai.com/v1",
    "defaultModel": "gpt-4o",
    "subagentModel": "gpt-4o-mini"
  },
  "agent": {
    "maxIterations": 20
  },
  "tools": {
    "restrictToWorkspace": true
  },
  "paths": {
    "dataRoot":     "/root/.nanosec",
    "sessionDir":   "/root/.nanosec/sessions",
    "cronFile":     "/root/.nanosec/cron.json",
    "mediaBaseDir": "/root/.nanosec/media",
    "workspaceDir": "/workspace",
    "identityFile": "/workspace/AGENT.md",
    "bootstrapDir": "/workspace/bootstrap",
    "memoryDir":    "/workspace/memory",
    "skillsDir":    "/workspace/skills"
  }
}
```

> The `__PLACEHOLDER__` values are replaced by the Jenkins pipeline using `sed` before writing the final configs to the deploy host. **Do not commit real secrets.**

### 1.5 `Jenkinsfile`

```groovy
pipeline {
    agent any

    environment {
        REGISTRY      = 'registry.example.com'          // your Docker registry host
        IMAGE_NAME    = "${REGISTRY}/nanosec/nanosec"
        IMAGE_TAG     = "${GIT_COMMIT[0..7]}"
        IMAGE         = "${IMAGE_NAME}:${IMAGE_TAG}"
        DEPLOY_HOST   = 'deploy@your-server.example.com'
        DEPLOY_DIR    = '/opt/nanosec'
    }

    stages {

        stage('Test') {
            steps {
                sh 'go test ./...'
            }
        }

        stage('Build & Push') {
            steps {
                withCredentials([usernamePassword(
                    credentialsId: 'docker-registry',
                    usernameVariable: 'REGISTRY_USER',
                    passwordVariable: 'REGISTRY_PASS'
                )]) {
                    sh '''
                        docker build \
                            --build-arg VERSION=${GIT_COMMIT[0..7]} \
                            --build-arg COMMIT=${GIT_COMMIT} \
                            --build-arg DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
                            -t ${IMAGE} .

                        echo "${REGISTRY_PASS}" | docker login ${REGISTRY} \
                            -u "${REGISTRY_USER}" --password-stdin

                        docker push ${IMAGE}
                        docker logout ${REGISTRY}
                    '''
                }
            }
        }

        stage('Deploy') {
            steps {
                withCredentials([
                    string(credentialsId: 'bus-db-dsn',         secretText: variable: 'BUS_DB_DSN'),
                    string(credentialsId: 'wecom-corp-id',       secretText: variable: 'WECOM_CORP_ID'),
                    string(credentialsId: 'wecom-corp-secret',   secretText: variable: 'WECOM_CORP_SECRET'),
                    string(credentialsId: 'wecom-agent-id',      secretText: variable: 'WECOM_AGENT_ID'),
                    string(credentialsId: 'wecom-token',         secretText: variable: 'WECOM_TOKEN'),
                    string(credentialsId: 'wecom-aes-key',       secretText: variable: 'WECOM_AES_KEY'),
                    string(credentialsId: 'llm-api-key',         secretText: variable: 'LLM_API_KEY'),
                ]) {
                    sshagent(['deploy-ssh-key']) {
                        sh '''
                            # 1. Prepare deploy directory
                            ssh -o StrictHostKeyChecking=no ${DEPLOY_HOST} \
                                "mkdir -p ${DEPLOY_DIR}/configs"

                            # 2. Copy compose file and template configs
                            scp docker-compose.prod.yml \
                                ${DEPLOY_HOST}:${DEPLOY_DIR}/docker-compose.yml
                            scp configs/*.json \
                                ${DEPLOY_HOST}:${DEPLOY_DIR}/configs/

                            # 3. Substitute secrets into configs on the remote host
                            ssh ${DEPLOY_HOST} bash -s <<ENDSSH
                                cd ${DEPLOY_DIR}/configs

                                sed -i "s|__BUS_DB_DSN__|${BUS_DB_DSN}|g"           bus.json
                                sed -i "s|__WECOM_CORP_ID__|${WECOM_CORP_ID}|g"     gateway.json
                                sed -i "s|__WECOM_CORP_SECRET__|${WECOM_CORP_SECRET}|g" gateway.json
                                sed -i "s|__WECOM_AGENT_ID__|${WECOM_AGENT_ID}|g"   gateway.json
                                sed -i "s|0\b|${WECOM_AGENT_ID}|"                   gateway.json
                                sed -i "s|__WECOM_TOKEN__|${WECOM_TOKEN}|g"         gateway.json
                                sed -i "s|__WECOM_AES_KEY__|${WECOM_AES_KEY}|g"     gateway.json
                                sed -i "s|__LLM_API_KEY__|${LLM_API_KEY}|g"         agent.json

                                chmod 600 *.json
ENDSSH

                            # 4. Pull and restart services
                            ssh ${DEPLOY_HOST} bash -s <<ENDSSH
                                cd ${DEPLOY_DIR}
                                IMAGE=${IMAGE} docker compose pull
                                IMAGE=${IMAGE} docker compose up -d --remove-orphans
ENDSSH
                        '''
                    }
                }
            }
        }

        stage('Run Migrations') {
            when {
                // Only run on main branch to avoid accidental migration on PRs
                branch 'main'
            }
            steps {
                withCredentials([
                    string(credentialsId: 'bus-db-dsn', secretText: variable: 'BUS_DB_DSN'),
                ]) {
                    sshagent(['deploy-ssh-key']) {
                        sh '''
                            scp migrations/*.sql ${DEPLOY_HOST}:/tmp/nanosec-migrations/
                            ssh ${DEPLOY_HOST} bash -s <<ENDSSH
                                for f in /tmp/nanosec-migrations/*.sql; do
                                    echo "Applying \$f"
                                    mysql "${BUS_DB_DSN}" < "\$f" 2>/dev/null || true
                                done
                                rm -rf /tmp/nanosec-migrations
ENDSSH
                        '''
                    }
                }
            }
        }
    }

    post {
        always {
            sh 'docker logout ${REGISTRY} 2>/dev/null || true'
        }
        success {
            echo "Deployed ${IMAGE} successfully."
        }
        failure {
            echo "Deployment failed. Check the logs above."
        }
    }
}
```

---

## Step 2 — Jenkins Credentials

In **Jenkins → Manage Jenkins → Credentials**, add the following **Secret text** entries. The IDs must match the `credentialsId` values in the Jenkinsfile exactly.

| Credential ID | Value |
|---------------|-------|
| `docker-registry` | Username + password for your Docker registry |
| `deploy-ssh-key` | SSH private key for the deploy user on the target host |
| `bus-db-dsn` | `nanosec:password@tcp(your-mysql-host:3306)/nanosec?parseTime=true` |
| `wecom-corp-id` | Enterprise ID from WeCom admin (e.g. `wwXXXXXXXXXXXX`) |
| `wecom-corp-secret` | App secret from WeCom admin |
| `wecom-agent-id` | Numeric agent ID from WeCom admin (e.g. `1000001`) |
| `wecom-token` | The Token you choose when configuring the callback |
| `wecom-aes-key` | The 43-character EncodingAESKey from WeCom admin |
| `llm-api-key` | Your OpenAI / OpenRouter / etc. API key |

---

## Step 3 — Database Setup

Run the migrations once against your existing MySQL. The pipeline's `Run Migrations` stage does this automatically on the `main` branch, but you can also run it manually:

```bash
mysql -h your-mysql-host -u nanosec -p nanosec < migrations/001_create_users.sql
mysql -h your-mysql-host -u nanosec -p nanosec < migrations/002_create_queues.sql
```

Then register each WeCom user. The `sender_id` must be the employee's **WeCom UserID** (found in the WeCom admin under 通讯录):

```sql
-- Repeat for each user
INSERT INTO users (id) VALUES ('alice')
  ON DUPLICATE KEY UPDATE updated_at = CURRENT_TIMESTAMP;

INSERT INTO user_channels (user_id, channel, sender_id)
  VALUES ('alice', 'wecom', 'zhangsan')   -- 'zhangsan' = WeCom UserID
  ON DUPLICATE KEY UPDATE user_id = VALUES(user_id);
```

---

## Step 4 — nginx on the Deploy Host

WeCom requires HTTPS for the callback URL. nginx terminates TLS and forwards to the gateway container, which is bound to `127.0.0.1:8081`.

```nginx
# /etc/nginx/sites-available/nanosec-wecom
server {
    listen 443 ssl;
    server_name your.domain.com;

    ssl_certificate     /etc/letsencrypt/live/your.domain.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/your.domain.com/privkey.pem;

    location /wecom/callback {
        proxy_pass        http://127.0.0.1:8081/callback;
        proxy_set_header  Host $host;
        proxy_read_timeout 10s;
    }
}

server {
    listen 80;
    server_name your.domain.com;
    return 301 https://$host$request_uri;
}
```

```bash
sudo ln -s /etc/nginx/sites-available/nanosec-wecom /etc/nginx/sites-enabled/
sudo nginx -t && sudo systemctl reload nginx
```

The callback URL you will register in WeCom:

```
https://your.domain.com/wecom/callback
```

---

## Step 5 — WeCom Admin Configuration

Log in to the [WeCom Admin Console](https://work.weixin.qq.com/wework_admin/frame).

**Collect credentials** (these go into Jenkins as described in Step 2):

| Jenkins credential | Where to find it |
|-------------------|-----------------|
| `wecom-corp-id` | 我的企业 → 企业信息 → 企业ID |
| `wecom-corp-secret` | 应用管理 → [your app] → Secret |
| `wecom-agent-id` | 应用管理 → [your app] → AgentId |

**Configure the callback** (接收消息 → 设置API接收):

| Field | Value |
|-------|-------|
| URL | `https://your.domain.com/wecom/callback` |
| Token | Any string you choose → save to `wecom-token` in Jenkins |
| EncodingAESKey | Click 随机生成 → save to `wecom-aes-key` in Jenkins |

> **Timing**: The gateway container must already be running when you click 保存, because WeCom sends an immediate GET verification request. Run the pipeline first (Step 6), then click 保存 in WeCom.

---

## Step 6 — First Deployment

1. Push the `Jenkinsfile`, `docker-compose.prod.yml`, and `configs/*.json` templates to your repository's `main` branch.
2. In Jenkins, create a new **Pipeline** job pointing to your repository.
3. Run the pipeline. It will:
   - Run tests
   - Build and push the Docker image
   - Copy files to the deploy host and inject secrets
   - Run `docker compose up -d`
   - Apply database migrations
4. After the pipeline succeeds, go to WeCom admin and click **保存** to trigger URL verification.

Check the gateway container log on the deploy host:

```bash
ssh deploy@your-server.example.com \
    "docker compose -f /opt/nanosec/docker-compose.yml logs -f nanogateway"
```

A successful start looks like:

```
level=INFO msg="wecom: channel started" addr=0.0.0.0:8081
```

---

## Step 7 — Verification

Send a message from WeCom to your app, then watch all three logs:

```bash
# On the deploy host
cd /opt/nanosec
docker compose logs -f
```

Expected log flow:

```
nanogateway  | level=INFO msg="wecom: message received" sender_id=zhangsan
nanobus      | level=INFO msg="message enqueued" user_id=alice
agent-alice  | level=INFO msg="agent processing" user_id=alice
agent-alice  | level=INFO msg="agent replied"
nanogateway  | level=INFO msg="sending outbound message" channel=wecom chat_id=zhangsan
```

---

## Adding More Users

To add a second user (e.g. `bob` with WeCom UserID `lisi`):

1. Register in MySQL:
   ```sql
   INSERT INTO users (id) VALUES ('bob')
     ON DUPLICATE KEY UPDATE updated_at = CURRENT_TIMESTAMP;
   INSERT INTO user_channels (user_id, channel, sender_id)
     VALUES ('bob', 'wecom', 'lisi')
     ON DUPLICATE KEY UPDATE user_id = VALUES(user_id);
   ```

2. Add a service block to `docker-compose.prod.yml`:
   ```yaml
   agent-bob:
     image: ${IMAGE}
     entrypoint: nanosec
     restart: unless-stopped
     command: ["--user", "bob", "--config", "/etc/nanosec/agent.json"]
     configs:
       - source: agent_config
         target: /etc/nanosec/agent.json
     volumes:
       - agent_bob_data:/workspace
     depends_on:
       nanobus:
         condition: service_healthy
     networks:
       - nanosec
   ```
   And add `agent_bob_data:` under `volumes:`.

3. Commit and push — the pipeline redeploys automatically.

---

## Troubleshooting

| Symptom | Check |
|---------|-------|
| Pipeline fails at **Build & Push** | Registry credentials in Jenkins; `docker login` on the Jenkins agent |
| Pipeline fails at **Deploy** | SSH key credential; `DEPLOY_HOST` reachable; `docker compose` installed on deploy host |
| WeCom URL verification fails | `docker compose logs nanogateway`; `curl -v https://your.domain.com/wecom/callback`; nginx config |
| `unknown user` in bus logs | Missing `user_channels` row for that WeCom UserID — run the SQL in Step 3 |
| Gateway starts but WeCom messages never arrive | Check the app is set as the WeCom app the user is messaging; check AgentID matches |
| `encodingAESKey` error on gateway start | Key must be exactly 43 characters — regenerate in WeCom admin |
| Agent container keeps restarting | Check `docker compose logs agent-alice`; likely LLM API key or bus connection issue |

---

## Revision History

| Version | Date | Changes |
|---------|------|---------|
| 1.0 | 2026-03-05 | Initial Jenkins + Docker guide (external MySQL) |
