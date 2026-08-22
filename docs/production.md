# Production Deployment Guide

This guide covers deploying `telesrv` in production using the prebuilt multi-arch images (`ghcr.io/roon9e/gramsrv:latest`), Docker Compose, and automated secret generation.

---

## 1. Quickstart (Deploy via `curl`)

You do **not** need to clone the full repository or install Go on your production server.

### Step 1: Download Deployment Files

Create the required directory structure and download all necessary files:

```bash
mkdir -p telesrv/postgres-init telesrv/configs/docker && cd telesrv

# Download main orchestration and configuration files
curl -sSL -o docker-compose.yml https://raw.githubusercontent.com/roon9e/gramsrv/v2/docker-compose.yml
curl -sSL -o .env.example https://raw.githubusercontent.com/roon9e/gramsrv/v2/.env.example

# Download database initialization
curl -sSL -o postgres-init/010_branch_databases.sql https://raw.githubusercontent.com/roon9e/gramsrv/v2/postgres-init/010_branch_databases.sql

# Install update catalog
mkdir -p data/updates/files
curl -sSL -o data/updates/manifest.json https://raw.githubusercontent.com/roon9e/gramsrv/v2/deploy/update/manifest.example.json

# Create production .env file
cp .env.example .env
```

### Step 1b: Download Microservice Configuration Files (Critical)

**This step is essential.** Each microservice requires a YAML configuration file that specifies logging, gRPC ports, database connections, and other runtime parameters.

```bash
# Download all microservice YAML configs
for file in admin.yaml core.yaml edge.yaml egress.yaml file.yaml sfu.yaml ton.yaml; do \
  curl -sSL -o "configs/docker/${file}" "https://raw.githubusercontent.com/roon9e/gramsrv/v2/configs/docker/${file}"; \
done
```

**Why this step is necessary:**

- The `docker-compose.yml` file specifies `command: ["telesrv-edge", "--config", "/app/configs/docker/edge.yaml"]` for each service.
- Each `.yaml` file configures service-specific behavior: logging levels, gRPC bind addresses, health check intervals, connection pooling, and more.
- Without these files, services will fail at startup with "config file not found" errors.
- Configs are mounted as read-only volumes: `./configs/docker:/app/configs/docker:ro`.

**Config substitution:**

Each YAML file contains environment variable placeholders (e.g., `${TELESRV_REDIS_ADDR}`, `${POSTGRES_PASSWORD}`) that are substituted from your `.env` file **by the containers at runtime**. This allows a single set of YAML templates to work across all deployments.

**Alternative (if manual download is preferred):**

Instead of the loop, you can download configs individually or via your preferred CI/CD system:
```bash
curl -sSL -o configs/docker/core.yaml https://raw.githubusercontent.com/roon9e/gramsrv/v2/configs/docker/core.yaml
curl -sSL -o configs/docker/edge.yaml https://raw.githubusercontent.com/roon9e/gramsrv/v2/configs/docker/edge.yaml
# ... repeat for admin.yaml, egress.yaml, file.yaml, sfu.yaml, ton.yaml
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

**Windows PowerShell Alternative:**

```powershell
Copy-Item .env.example .env
$pgPw = -join ((1..32) | ForEach-Object { '{0:x}' -f (Get-Random -Max 16) })
$adminPw = -join ((1..24) | ForEach-Object { [char](Get-Random -Min 65 -Max 122) })
(Get-Content .env) `
  -replace '^POSTGRES_PASSWORD=.*', "POSTGRES_PASSWORD=$pgPw" `
  -replace '^TELESRV_CORE_EXEC_TOKEN=.*', "TELESRV_CORE_EXEC_TOKEN=$(-join ((1..48) | ForEach-Object { '{0:x}' -f (Get-Random -Max 16) }))" `
  -replace '^TELESRV_FILE_TOKEN=.*', "TELESRV_FILE_TOKEN=$(-join ((1..48) | ForEach-Object { '{0:x}' -f (Get-Random -Max 16) }))" `
  -replace '^TELESRV_EGRESS_ACK_TOKEN=.*', "TELESRV_EGRESS_ACK_TOKEN=$(-join ((1..48) | ForEach-Object { '{0:x}' -f (Get-Random -Max 16) }))" `
  -replace '^TELESRV_GROUPCALL_CONTROL_TOKEN=.*', "TELESRV_GROUPCALL_CONTROL_TOKEN=$(-join ((1..48) | ForEach-Object { '{0:x}' -f (Get-Random -Max 16) }))" `
  -replace '^TELESRV_SFU_CONTROL_TOKEN=.*', "TELESRV_SFU_CONTROL_TOKEN=$(-join ((1..48) | ForEach-Object { '{0:x}' -f (Get-Random -Max 16) }))" `
  -replace '^TELESRV_ADMIN_API_TOKEN=.*', "TELESRV_ADMIN_API_TOKEN=$(-join ((1..48) | ForEach-Object { '{0:x}' -f (Get-Random -Max 16) }))" `
  -replace '^TELESRV_ADMIN_UI_PASSWORD=.*', "TELESRV_ADMIN_UI_PASSWORD=$adminPw" `
  -replace '^TELESRV_ADMIN_SESSION_KEY=.*', "TELESRV_ADMIN_SESSION_KEY=$(-join ((1..64) | ForEach-Object { '{0:x}' -f (Get-Random -Max 16) }))" |
  Set-Content .env -Encoding UTF8
Write-Host "Generated random Admin UI password: $adminPw"
```

### Step 3: Configure Network & Public IP

Edit `.env` and configure:
- `TELESRV_ADVERTISE_IP`: The public IP or reachable LAN IP of your server.
- `TELESRV_PUBLIC_BASE_URL`: Public HTTPS/HTTP root (e.g. `https://telesrv.example.com`).

See [configuration.en.md](configuration.en.md) (Section 1) for a complete reference of all network settings.

### Step 4: Start the Stack

```bash
docker compose up -d
```

Verify all containers are running:
```bash
docker compose ps
```

Expected output (simplified):
```
NAME                    STATUS              PORTS
telesrv-postgres        Up (healthy)        127.0.0.1:5432->5432/tcp
telesrv-redis           Up (healthy)        127.0.0.1:6399->6379/tcp
telesrv-file            Up                  (no public ports)
telesrv-core            Up                  0.0.0.0:2401->2401/tcp, 0.0.0.0:8081->8081/tcp
telesrv-edge            Up                  0.0.0.0:2398->2398/tcp
telesrv-egress          Up                  (no public ports)
telesrv-sfu             Up                  0.0.0.0:12399->12399/udp
telesrv-admin           Up                  127.0.0.1:2600->2600/tcp
```

Check logs:
```bash
docker compose logs -f telesrv-edge        # Watch client connection gateway
docker compose logs -f telesrv-core        # Watch RPC dispatcher
docker compose logs -f telesrv-admin       # Watch admin UI server
```

---

## 2. Configuration Files & Environment Variables

### Directory Structure
After following Step 1, your deployment directory should look like:

```
telesrv/
├── docker-compose.yml                     # Service definitions
├── .env.example                           # Template (reference only)
├── .env                                   # Your production configuration
├── postgres-init/
│   └── 010_branch_databases.sql           # PostgreSQL initialization
├── configs/docker/                        # Service YAML configs (required)
│   ├── core.yaml
│   ├── edge.yaml
│   ├── file.yaml
│   ├── egress.yaml
│   ├── admin.yaml
│   ├── sfu.yaml
│   └── ton.yaml
└── data/                                  # Persistent state (created on first run)
    ├── server_rsa.pem                     # MTProto RSA private key (auto-generated)
    ├── server_rsa.pub                     # Public key for clients
    ├── blobs/                             # Uploaded media files
    ├── langpack/                          # Language packs
    ├── official-gifts/                    # Star Gift catalogs
    └── ...
```

### Environment Variables (`.env`)

All service configuration is **environment-driven** via the `.env` file and Docker `env_file` directive in `docker-compose.yml`. The containers read `.env` and substitute placeholders in the YAML configs at startup.

**Key sections:**

- **Database & Cache**: `POSTGRES_PASSWORD`, `TELESRV_REDIS_ADDR`, etc.
- **Security Tokens**: `TELESRV_CORE_EXEC_TOKEN`, `TELESRV_FILE_TOKEN`, etc. (must be identical across services that communicate).
- **Network**: `TELESRV_ADVERTISE_IP`, `TELESRV_PUBLIC_BASE_URL`, port mappings.
- **Feature Flags**: `TELESRV_AI_ENABLED`, `TELESRV_LIVESTREAM_ENABLE`, etc.

For all available options, see [configuration.en.md](configuration.en.md).

### Microservice YAML Configs

Each YAML config file (e.g., `core.yaml`, `edge.yaml`) is a **service-specific blueprint** that gets substituted with environment variables and parsed by the service at startup.

**Example: `core.yaml` excerpt**
```yaml
app:
  id: core
  coreExec:
    addr: ${TELESRV_CORE_EXEC_GRPC_ADDR:-0.0.0.0:2440}
    token: ${TELESRV_CORE_EXEC_TOKEN}
  groupCall:
    addr: ${TELESRV_GROUPCALL_CONTROL_ADDR:-0.0.0.0:2420}
```

At runtime, `docker-compose` mounts `./configs/docker/` into the container as `/app/configs/docker/` (read-only), and the service reads the file with all `${VAR}` placeholders replaced by the corresponding environment variable from `.env`.

**When to modify YAML configs:**

- **Rarely.** Most production customization is done via `.env`.
- **Advanced use cases**: Custom logging formats, extended health check intervals, performance tuning, or adding experimental features.
- **After modification**: Restart the affected container: `docker compose restart telesrv-core`.

### Identity migration

The normal service startup validates identity files but does not perform
destructive migrations. To convert an older PKCS#8 RSA key explicitly, run:

```bash
docker run --rm \
   -v "$PWD/data:/data" \
   telesrv:latest \
   telesrv-key-migrate \
   -in /data/server_rsa.pem \
   -out /data/server_rsa.pem
```

The command creates `/data/server_rsa.pem.before-pkcs1` before replacing the
key. Back up the data directory first and restart the MTProto services after
the migration. Service startup validates the key but does not migrate it
automatically.

---

## 3. Network Security Architecture & UFW Safety

Docker directly configures `iptables` forwarding rules, which bypasses standard `UFW` rules on Linux hosts. Telesrv implements defense-in-depth isolation:

### Private Docker Network (`telesrv-net`)

All microservices communicate over a private bridge network. Internal gRPC endpoints have **no published host ports**:

| Service | Internal Port | Purpose | Public? |
|---|---|---|---|
| telesrv-core | `2440` (gRPC) | CoreExec RPC | ❌ Internal only |
| telesrv-file | `2520` (gRPC) | FileData RPC | ❌ Internal only |
| telesrv-egress | `2510` (gRPC) | Egress ACK writeback | ❌ Internal only |
| telesrv-sfu | `2450` (gRPC) | SFU control | ❌ Internal only |
| telesrv-core | `2420` (HTTP) | GroupCall control | ❌ Internal only |
| telesrv-admin | `2601` (gRPC) | Admin API | ❌ Internal only |
| postgres | `5432` | Database | ❌ `127.0.0.1` only |
| redis | `6379` | Cache | ❌ `127.0.0.1` only |

### Client-Facing Public Ports

These ports are published to the host per `.env` settings:

| Service | Port | Protocol | Default Bind | Purpose | Setting |
|---|---|---|---|---|---|
| telesrv-edge | `2398` | TCP | `0.0.0.0` | MTProto & WebSocket | `TELESRV_EDGE_PORT` |
| telesrv-sfu | `12399` | UDP | `0.0.0.0` | WebRTC media | `TELESRV_SFU_UDP_PORT` |
| telesrv-core | `2401` | TCP | `0.0.0.0` | Public links & login | `TELESRV_PUBLIC_LINK_PORT` |
| telesrv-core | `8081` | TCP | `0.0.0.0` | Bot API HTTP | `TELESRV_BOT_API_PORT` |
| telesrv-update | `2402` | TCP | `0.0.0.0` | Auto-update CDN | `TELESRV_UPDATE_PORT` |
| telesrv-admin | `2600` | TCP | `127.0.0.1` | Admin web UI | `TELESRV_ADMIN_UI_PORT` |

**Firewall Strategy:**

- Open only required public ports: `2398/tcp`, `12399/udp`, `2401/tcp`, `8081/tcp`, `2402/tcp` (optional).
- Keep admin UI (`2600`) bound to `127.0.0.1` and access via SSH tunnel: `ssh -L 2600:127.0.0.1:2600 user@server`.
- Database and cache ports (`5432`, `6379`) bound to `127.0.0.1`; access for maintenance via SSH tunnel if needed.

### UFW Configuration (Example)

```bash
sudo ufw default deny incoming
sudo ufw default allow outgoing
sudo ufw allow 22/tcp                    # SSH
sudo ufw allow 2398/tcp                  # telesrv-edge (MTProto)
sudo ufw allow 12399/udp                 # telesrv-sfu (WebRTC)
sudo ufw allow 2401/tcp                  # telesrv-core (public links)
sudo ufw allow 8081/tcp                  # telesrv-core (bot API)
# sudo ufw allow 2402/tcp                # telesrv-update (optional)
sudo ufw enable
```

---

## 4. Host `./data` Directory Management

All stateful data is mounted directly from `./data` on your host to `/app/data` inside containers. This includes uploaded media, cache, and dynamically generated artifacts.

### Automatic Initialization

On first startup, `docker-entrypoint.sh` in each container:
1. Generates the MTProto RSA key pair (`server_rsa.pem`, `server_rsa.pub`).
2. Creates required subdirectories.
3. Validates secret tokens are set.

### Public RSA Key for Clients

After first startup, retrieve the public key:
```bash
cat data/server_rsa.pub
```

Share this public key and your `TELESRV_ADVERTISE_IP:2398` (DC `2`) with patched Telegram clients. The client uses these to establish MTProto connections.

### Custom Seed Assets

You can pre-populate seed data by placing files into `./data/` before or after startup:

| Directory | Purpose | Note |
|---|---|---|
| `./data/official-gifts/` | Star Gift catalogs (JSON) | Loaded on container startup |
| `./data/sticker-seed/` | Custom sticker packs | Imported by `stickerfetch` service or API |
| `./data/premium-promo/` | Premium feature videos/images | Served by web UI |
| `./data/langpack/` | Language `.strings` files | Synced to clients |
| `./data/gifs/` | GIF/animation seeds | For GIF search/trending |
| `./data/maptiles/` | Map tile cache | Mapbox-compatible MBTiles |

**Adding assets after startup:**
```bash
# Example: Add custom sticker pack
curl -o data/sticker-seed/my-pack.json https://example.com/pack.json
# Services will pick up new files on next sync/fetch cycle
```

### Data Persistence & Backups

The Docker volumes are defined as:
```yaml
volumes:
  pgdata:      # PostgreSQL data directory
  redisdata:   # Redis persistence (AOF)
```

These are backed by the Docker engine and typically stored in `/var/lib/docker/volumes/`. For production:

**Backup PostgreSQL:**
```bash
docker compose exec postgres pg_dump -U telesrv telesrv | gzip > backup-$(date +%Y%m%d).sql.gz
```

**Backup Redis:**
```bash
docker compose exec redis redis-cli BGSAVE
# Copy /var/lib/docker/volumes/telesrv_redisdata/_data/dump.rdb
```

**Backup media files:**
```bash
tar -czf backup-data-$(date +%Y%m%d).tar.gz data/
```

---

## 5. Troubleshooting Deployment Issues

### Services fail to start: "config file not found"

**Cause:** Missing `configs/docker/` directory or incomplete file downloads.

**Solution:**
```bash
# Re-run Step 1b to download all config files
for file in admin.yaml core.yaml edge.yaml egress.yaml file.yaml sfu.yaml ton.yaml; do \
  curl -sSL -o "configs/docker/${file}" "https://raw.githubusercontent.com/roon9e/gramsrv/v2/configs/docker/${file}"; \
done

# Verify files exist
ls -la configs/docker/

# Restart services
docker compose restart
```

### Services fail with "invalid token" or "authentication failed"

**Cause:** Token mismatch between services (e.g., `TELESRV_CORE_EXEC_TOKEN` differs between telesrv-core and telesrv-edge).

**Solution:**
1. Verify tokens are identical in `.env`:
   ```bash
   grep TELESRV_.*_TOKEN .env | sort
   ```
2. Regenerate if necessary:
   ```bash
   CORE_TOKEN=$(openssl rand -hex 24)
   sed -i "s/^TELESRV_CORE_EXEC_TOKEN=.*/TELESRV_CORE_EXEC_TOKEN=${CORE_TOKEN}/" .env
   ```
3. Restart all services:
   ```bash
   docker compose restart
   ```

### PostgreSQL connection refused

**Cause:** Incorrect DSN or PostgreSQL not ready.

**Solution:**
1. Check `.env` has the correct DSN:
   ```bash
   grep TELESRV_POSTGRES_DSN .env
   # Should be: postgres://telesrv:PASSWORD@postgres:5432/telesrv?sslmode=disable
   ```
2. Wait for PostgreSQL to be healthy:
   ```bash
   docker compose ps postgres
   # Should show "Up (healthy)"
   ```
3. Check logs:
   ```bash
   docker compose logs postgres
   ```

### Admin UI not accessible

**Cause:** Port binding or container not running.

**Solution:**
```bash
# Check container is running
docker compose ps telesrv-admin

# Verify port binding
docker compose port telesrv-admin 2600
# Should output: 127.0.0.1:2600

# Access via SSH tunnel
ssh -L 2600:127.0.0.1:2600 user@server
# Then open http://localhost:2600 in your browser

# Check logs
docker compose logs telesrv-admin
```

### High CPU/Memory usage

**Cause:** Service misconfiguration, memory leaks, or insufficient resources.

**Solution:**
1. Monitor resource usage:
   ```bash
   docker stats telesrv-core telesrv-edge telesrv-sfu
   ```
2. Check service logs for errors:
   ```bash
   docker compose logs telesrv-core | tail -100
   ```
3. Review `.env` settings:
   - `TELESRV_OUTBOX_WORKERS`: Reduce if CPU-bound.
   - `TELESRV_POSTGRES_MAX_CONNS`: Reduce if connection pool is exhausted.
   - `TELESRV_SFU_INSTANCE_MAX_ACTIVE_CALLS`: Limit concurrent SFU calls.
4. Increase host resources (vCPU, RAM) if baseline is insufficient.

---

## 6. Scaling & Multi-Server Deployments

For production clusters with multiple servers, consider:

### Shared PostgreSQL/Redis

Use managed services (AWS RDS, Google Cloud SQL, Redis Enterprise) or a separate HA cluster:
- Update `TELESRV_POSTGRES_DSN` to point to the remote database.
- Update `TELESRV_REDIS_ADDR` to point to the remote cache.

### Multiple Edge/SFU Instances

Deploy `telesrv-edge` and `telesrv-sfu` on separate servers for horizontal scalability:
- Core and Egress can run on a single shared server (shared PostgreSQL/Redis).
- Update `TELESRV_CORE_EXEC_GRPC_TARGETS`, `TELESRV_SFU_CONTROL_GRPC_TARGETS` to target the shared instances.

### Load Balancing

- Place a reverse proxy (nginx, HAProxy) in front of `telesrv-edge:2398` for TCP load balancing.
- For `telesrv-sfu:12399/udp`, use DNS round-robin or application-level client steering via `TELESRV_ADVERTISE_IP`.

---

## 7. Development Workflow

To build and test images locally from source:

```bash
git clone https://github.com/roon9e/gramsrv.git
cd gramsrv

# Build multi-arch images for local testing
docker compose -f docker-compose-development.yml build

# Start the stack with locally built images
docker compose -f docker-compose-development.yml up -d

# Watch logs
docker compose -f docker-compose-development.yml logs -f
```

For more details, see [local-setup.md](local-setup.md).

---

## References

- [configuration.en.md](configuration.en.md) — Complete reference of all `.env` settings.
- [README.md](../README.md) — Overview and architecture.
- [local-setup.md](local-setup.md) — Development setup guide.
