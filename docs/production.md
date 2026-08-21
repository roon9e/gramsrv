# Production Deployment Guide

This guide covers deploying `telesrv` in production using the prebuilt multi-arch images (`ghcr.io/roon9e/gramsrv:latest`), Docker Compose, and automated secret generation.

---

## 1. Quickstart (Deploy via `curl`)

You do **not** need to clone the full repository or install Go on your production server.

### Step 1: Download Deployment Files

```bash
mkdir -p telesrv/postgres-init && cd telesrv

# Download production compose file, environment template, and database init script
curl -sSL -o docker-compose.yml https://raw.githubusercontent.com/roon9e/gramsrv/v2/docker-compose.yml
curl -sSL -o .env.example https://raw.githubusercontent.com/roon9e/gramsrv/v2/.env.example
curl -sSL -o postgres-init/010_branch_databases.sql https://raw.githubusercontent.com/roon9e/gramsrv/v2/postgres-init/010_branch_databases.sql

# Create production .env file
cp .env.example .env
```

### Step 2: Generate Cryptographic Secrets via `sed`

Run this automated one-liner to generate high-entropy, random secrets for all databases, inter-service tokens, and admin session keys:

```bash
POSTGRES_PW=$(openssl rand -hex 16)
ADMIN_PW=$(openssl rand -base64 16)

sed -i \
  -e "s/^POSTGRES_PASSWORD=.*/POSTGRES_PASSWORD=${POSTGRES_PW}/" \
  -e "s/^TELESRV_CORE_EXEC_TOKEN=.*/TELESRV_CORE_EXEC_TOKEN=$(openssl rand -hex 24)/" \
  -e "s/^TELESRV_FILE_TOKEN=.*/TELESRV_FILE_TOKEN=$(openssl rand -hex 24)/" \
  -e "s/^TELESRV_EGRESS_ACK_TOKEN=.*/TELESRV_EGRESS_ACK_TOKEN=$(openssl rand -hex 24)/" \
  -e "s/^TELESRV_GROUPCALL_CONTROL_TOKEN=.*/TELESRV_GROUPCALL_CONTROL_TOKEN=$(openssl rand -hex 24)/" \
  -e "s/^TELESRV_SFU_CONTROL_TOKEN=.*/TELESRV_SFU_CONTROL_TOKEN=$(openssl rand -hex 24)/" \
  -e "s/^TELESRV_ADMIN_API_TOKEN=.*/TELESRV_ADMIN_API_TOKEN=$(openssl rand -hex 24)/" \
  -e "s/^TELESRV_ADMIN_UI_PASSWORD=.*/TELESRV_ADMIN_UI_PASSWORD=${ADMIN_PW}/" \
  -e "s/^TELESRV_ADMIN_SESSION_KEY=.*/TELESRV_ADMIN_SESSION_KEY=$(openssl rand -hex 32)/" \
  .env

echo "Generated random Admin UI password: ${ADMIN_PW}"
```

> **Windows PowerShell Alternative:**
> ```powershell
> Copy-Item .env.example .env
> $pgPw = -join ((1..32) | ForEach-Object { '{0:x}' -f (Get-Random -Max 16) })
> $adminPw = -join ((1..24) | ForEach-Object { [char](Get-Random -Min 65 -Max 122) })
> (Get-Content .env) `
>   -replace '^POSTGRES_PASSWORD=.*', "POSTGRES_PASSWORD=$pgPw" `
>   -replace '^TELESRV_CORE_EXEC_TOKEN=.*', "TELESRV_CORE_EXEC_TOKEN=$(-join ((1..48) | ForEach-Object { '{0:x}' -f (Get-Random -Max 16) }))" `
>   -replace '^TELESRV_FILE_TOKEN=.*', "TELESRV_FILE_TOKEN=$(-join ((1..48) | ForEach-Object { '{0:x}' -f (Get-Random -Max 16) }))" `
>   -replace '^TELESRV_EGRESS_ACK_TOKEN=.*', "TELESRV_EGRESS_ACK_TOKEN=$(-join ((1..48) | ForEach-Object { '{0:x}' -f (Get-Random -Max 16) }))" `
>   -replace '^TELESRV_GROUPCALL_CONTROL_TOKEN=.*', "TELESRV_GROUPCALL_CONTROL_TOKEN=$(-join ((1..48) | ForEach-Object { '{0:x}' -f (Get-Random -Max 16) }))" `
>   -replace '^TELESRV_SFU_CONTROL_TOKEN=.*', "TELESRV_SFU_CONTROL_TOKEN=$(-join ((1..48) | ForEach-Object { '{0:x}' -f (Get-Random -Max 16) }))" `
>   -replace '^TELESRV_ADMIN_API_TOKEN=.*', "TELESRV_ADMIN_API_TOKEN=$(-join ((1..48) | ForEach-Object { '{0:x}' -f (Get-Random -Max 16) }))" `
>   -replace '^TELESRV_ADMIN_UI_PASSWORD=.*', "TELESRV_ADMIN_UI_PASSWORD=$adminPw" `
>   -replace '^TELESRV_ADMIN_SESSION_KEY=.*', "TELESRV_ADMIN_SESSION_KEY=$(-join ((1..64) | ForEach-Object { '{0:x}' -f (Get-Random -Max 16) }))" |
>   Set-Content .env -Encoding UTF8
> Write-Host "Generated random Admin UI password: $adminPw"
> ```

### Step 3: Configure Network & Public IP

Edit `.env` and configure:
- `TELESRV_ADVERTISE_IP`: The public IP or reachable LAN IP of your server.
- `TELESRV_PUBLIC_BASE_URL`: Public HTTPS/HTTP root (e.g. `https://telesrv.example.com`).

### Step 4: Start the Stack

```bash
docker compose up -d
```

Check status and logs:
```bash
docker compose ps
docker compose logs -f
```

---

## 2. Network Security Architecture & UFW Safety

Docker directly configures `iptables` forwarding rules, which bypasses standard `UFW` rules on Linux hosts. Telesrv implements defense-in-depth isolation:

1. **Private Docker Network (`telesrv-net`)**:
   - Internal microservice communications (CoreExec gRPC `2440`, FileData gRPC `2520`, Egress ACK gRPC `2510`, SFU Control gRPC `2450`, GroupCall Control HTTP `2420`, and Admin API `2599`) have **no published host ports**.
   - PostgreSQL (`5432`) and Redis (`6379`) are bound to `127.0.0.1` by default and are not exposed over external interfaces.
2. **Configurable Interface Binding**:
   - `TELESRV_LISTEN_IP=0.0.0.0`: Public client-facing edge ports (`2398`, `12399/udp`, `2401`, `8088`, `2402`).
   - `TELESRV_LOCAL_LISTEN_IP=127.0.0.1`: Sensitive administrative dashboard (`2600`) and database debugging ports.

| Service | Container Port | Host Port Setting | Default Bind | Purpose |
|---|---|---|---|---|
| `telesrv-edge` | `2398` TCP | `${TELESRV_EDGE_PORT:-2398}` | `0.0.0.0` | MTProto TCP & WebSocket Gateway |
| `telesrv-sfu` | `12399` UDP | `${TELESRV_SFU_UDP_PORT:-12399}` | `0.0.0.0` | WebRTC SFU Media (Voice & Video) |
| `telesrv-core` | `2401` TCP | `${TELESRV_PUBLIC_LINK_PORT:-2401}` | `0.0.0.0` | Public Links & Telegram Login OIDC |
| `telesrv-core` | `8088` TCP | `${TELESRV_BOT_API_PORT:-8088}` | `0.0.0.0` | HTTP Bot API Gateway |
| `telesrv-update` | `2402` TCP | `${TELESRV_UPDATE_PORT:-2402}` | `0.0.0.0` | Desktop Auto-Update CDN |
| `telesrv-admin` | `2600` TCP | `${TELESRV_ADMIN_UI_PORT:-2600}` | `127.0.0.1` | Admin Web Dashboard |
| `postgres` | `5432` TCP | `${POSTGRES_PORT:-5432}` | `127.0.0.1` | Database Debugging / Migration |
| `redis` | `6379` TCP | `${REDIS_PORT:-6399}` | `127.0.0.1` | Cache Debugging |

---

## 3. Host `./data` Directory Management

All stateful data is mounted directly from `./data` on your host to `/app/data` inside containers.

### Public RSA Key for Clients
On first startup, the container automatically generates the MTProto RSA private key and exports the corresponding public key:
```bash
cat data/server_rsa.pub
```
Use this public key and your `TELESRV_ADVERTISE_IP:2398` (DC `2`) in patched Telegram clients.

### Adding Custom Seed Assets
You can place seed assets directly into the `./data/` folder on your host:
- `./data/official-gifts/`: Curated Star Gifts catalogs.
- `./data/sticker-seed/`: Custom sticker packs.
- `./data/premium-promo/`: Videos/images for Premium feature promotions.
- `./data/langpack/`: Custom client translation `.strings` files.

---

## 4. Development Workflow

To compile and build images locally from source:

```bash
docker compose -f docker-compose-development.yml up --build -d
```
