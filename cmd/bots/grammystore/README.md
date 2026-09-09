# grammystore

grammY-based Telegram authentication and store bot for Telesrv. Runs
independently from the main MTProto server; a bot outage cannot stop MTProto.

## Functionality

- automatic persistent number and initial code on `/start` (random mode);
- real-number mode: users bind their actual Telegram phone via contact sharing;
- delivery and storage of real login codes through authenticated `POST /code`;
- free replacement numbers and paid anonymous `+888` numbers (random mode);
- Premium, server Stars and collectible username purchases through Telegram Stars;
- arbitrary Stars invoices, payment deduplication and a durable sales journal;
- compensated refunds that revoke the exact Stars, Premium entitlement,
  collectible username or paid number before returning Telegram Stars;
- account target IDs and three recent recipients;
- daily bonuses, referrals and a weighted wheel;
- promo codes and button-based giveaways;
- support tickets;
- complete Russian and English localization;
- per-user language and notification settings;
- owner-only statistics, broadcasts, Stars/Premium/bonus grants, invoices,
  payment refunds, login-code access, support replies, Stars-rate controls;
- admin exact lookup by Telegram ID or phone number (no data dumps);
- optional required-channel membership gate.

## Prerequisites

- Docker and Docker Compose v2+
- A Telegram Bot token (from @BotFather)
- A gramsrv Admin API instance with a bearer token
- PostgreSQL (managed by Docker Compose)

## Installation

```bash
sudo mkdir -p /opt/grammystore
cd /opt/grammystore
```

Create `.env` from the template:

```bash
curl -o .env https://raw.githubusercontent.com/iamxvbaba/gramsrv/main/cmd/bots/grammystore/.env.example
nano .env
```

### Required environment variables

| Variable | Description |
|---|---|
| `BOT_TOKEN` | Telegram Bot API token from @BotFather |
| `OWNER_IDS` | Comma-separated Telegram user IDs for admin access |
| `GRAMSRV_TOKEN` | Bearer token for the gramsrv Admin API |
| `PUBLIC_BASE_URL` | Public base URL for the gramsrv instance |
| `CODE_WEBHOOK_SECRET` | HMAC secret for OTP webhook verification (min 24 chars) |
| `POSTGRES_PASSWORD` | PostgreSQL password (set a strong value) |
| `BOT_MODE` | `random` or `real` — controls number generation mode |

All required values must not be empty or contain placeholder values such as
`CHANGE_ME`, `YOUR_*`, `<...>`, or `example`. The application refuses to start
if any required variable is missing or still contains a placeholder.

### Bot modes

**Random mode** (`BOT_MODE=random`, default):
- Users receive a randomly generated phone number on `/start`.
- Each Telegram user has at most one active number at a time.
- Numbers persist across bot restarts.

**Real mode** (`BOT_MODE=real`):
- Users must bind their actual Telegram phone number via contact sharing.
- Random number generation is rejected server-side.
- The bound phone is stored in PostgreSQL and survives restarts.

### Production deployment (GHCR image)

```bash
docker compose pull
docker compose up -d
```

The production `docker-compose.yml` pulls the pre-built image from
`ghcr.io/iamxvbaba/gramsrv/grammystore:main`. No local build is required.

### Development deployment (local build)

```bash
docker compose -f docker-compose-dev.yml --env-file .env up -d --build
```

The development compose file builds the image locally from the Dockerfile.

## Management

```bash
# Start
docker compose up -d

# Stop
docker compose down

# Restart
docker compose restart

# View logs
docker compose logs -f grammystore

# Pull latest production image
docker compose pull
docker compose up -d

# Check health
curl http://localhost:2800/healthz
```

## PostgreSQL persistence

All data is stored in a PostgreSQL container with a named volume (`pgdata`).
The schema is initialized automatically from `db/init.sql` on first start.

Back up the database:

```bash
docker compose exec postgres pg_dump -U grammystore grammystore > backup.sql
```

Restore:

```bash
docker compose exec -T postgres psql -U grammystore grammystore < backup.sql
```

## Login-code webhook

Configure gramsrv's code-delivery webhook for:

```text
TELESRV_PHONE_CODE_DELIVERY_PROVIDER=webhook
TELESRV_OTP_WEBHOOK_URL=http://grammystore:2800/v1/otp/deliveries
TELESRV_OTP_WEBHOOK_SECRET=<same value as CODE_WEBHOOK_SECRET>
```

The endpoint verifies the HMAC-SHA256 signature, a five-minute timestamp
window and `Idempotency-Key`. It returns HTTP 202 immediately; Telegram
delivery then runs asynchronously. `/healthz` is read-only.

## Admin lookup

Administrators can look up specific codes/numbers through the admin panel:

1. Open the admin panel (`/admin`).
2. Press "Lookup".
3. Enter a phone number (with `+`) or a Telegram ID.

The lookup returns only the data for that exact query. There is no command
that dumps or lists all stored codes or numbers.

## Migration from the old systemd deployment

If you are migrating from the previous SQLite-based systemd deployment:

1. **Stop the old bot:**
   ```bash
   sudo systemctl stop telesrv-grammy-bot
   ```

2. **Back up the old SQLite database:**
   ```bash
   sudo cp /var/lib/gramsrv-grammy-bot/bot.sqlite3 /tmp/grammystore-backup.sqlite3
   ```

3. **Configure the new `.env`** as described above. Set `BOT_MODE` to match
   your previous configuration.

4. **Deploy the Docker stack:**
   ```bash
   cd /opt/grammystore
   docker compose pull
   docker compose up -d
   ```

5. **Verify the deployment:**
   ```bash
   docker compose logs -f grammystore
   curl http://localhost:2800/healthz
   ```

6. **Remove the old systemd service:**
   ```bash
   sudo systemctl disable telesrv-grammy-bot
   sudo rm /etc/systemd/system/telesrv-grammy-bot.service
   sudo rm /etc/telesrv-grammy-bot.env
   sudo rm -rf /opt/gramsrv-grammy-bot
   sudo systemctl daemon-reload
   ```

The old SQLite database format is not directly compatible with PostgreSQL.
Existing users will need to re-register with the bot. Login codes and
number assignments will be重新 generated on the first `/start`.

## Container image

The GHCR image coexists with the main server images under the same package:

- `ghcr.io/iamxvbaba/gramsrv/server`
- `ghcr.io/iamxvbaba/gramsrv/admin`
- `ghcr.io/iamxvbaba/gramsrv/grammystore`

All three images are published from the same repository.
