#!/bin/sh
set -e

# ==============================================================================
# Telesrv Container Entrypoint Script
# ==============================================================================

# Ensure data directories exist on the host/volume
mkdir -p \
  /app/data/blobs \
  /app/data/blob-staging \
  /app/data/telegram-login \
  /app/data/updates/files \
  /app/data/maptiles \
  /app/data/official-gifts \
  /app/data/sticker-seed \
  /app/data/premium-promo \
  /app/data/langpack

# ------------------------------------------------------------------------------
# 1. First-Run RSA Key Generation (server_rsa.pem & server_rsa.pub)
# ------------------------------------------------------------------------------
if [ ! -f "/app/data/server_rsa.pem" ]; then
  echo "[telesrv-entrypoint] Generating initial MTProto RSA private key..."
  openssl genrsa -out /app/data/server_rsa.pem 2048
  chmod 600 /app/data/server_rsa.pem || true
  
  echo "[telesrv-entrypoint] Extracting server RSA public key (for client configuration)..."
  openssl rsa -in /app/data/server_rsa.pem -pubout -out /app/data/server_rsa.pub
  echo "[telesrv-entrypoint] Server RSA public key saved to /app/data/server_rsa.pub"
fi

# ------------------------------------------------------------------------------
# 2. First-Run Telegram Login / OIDC Keys Provisioning
# ------------------------------------------------------------------------------
if [ ! -f "/app/data/telegram-login/signing-keys.json" ]; then
  echo "[telesrv-entrypoint] Initializing Telegram Login OIDC key ring..."
  if command -v telegramloginkeygen >/dev/null 2>&1; then
    telegramloginkeygen -mode init -dir /app/data/telegram-login || {
      echo "[telesrv-entrypoint] Warning: telegramloginkeygen initialization failed."
    }
  fi
fi

# ------------------------------------------------------------------------------
# 3. Security & Password Validation (Fail-Fast)
# ------------------------------------------------------------------------------
validate_secret() {
  var_name="$1"
  var_value="$2"
  if [ -z "$var_value" ] || [ "$var_value" = "CHANGEME" ]; then
    echo "[FATAL] Required configuration secret '$var_name' is missing or unconfigured."
    echo "        Please set secure random secrets in .env before launching."
    echo "        See docs/production.md for the automated secret generation command."
    exit 1
  fi
}

cmd_name="$(basename "$1" 2>/dev/null || echo "$1")"

case "$cmd_name" in
  telesrv-core|telesrv-egress|telesrv-file|telesrv-admin|telesrv-ton)
    validate_secret "POSTGRES_PASSWORD" "$POSTGRES_PASSWORD"
    ;;
esac

case "$cmd_name" in
  telesrv-core|telesrv-edge)
    validate_secret "TELESRV_CORE_EXEC_TOKEN" "$TELESRV_CORE_EXEC_TOKEN"
    validate_secret "TELESRV_FILE_TOKEN" "$TELESRV_FILE_TOKEN"
    ;;
esac

case "$cmd_name" in
  telesrv-edge|telesrv-egress)
    validate_secret "TELESRV_EGRESS_ACK_TOKEN" "$TELESRV_EGRESS_ACK_TOKEN"
    ;;
esac

case "$cmd_name" in
  telesrv-core|telesrv-sfu)
    validate_secret "TELESRV_GROUPCALL_CONTROL_TOKEN" "$TELESRV_GROUPCALL_CONTROL_TOKEN"
    validate_secret "TELESRV_SFU_CONTROL_TOKEN" "$TELESRV_SFU_CONTROL_TOKEN"
    ;;
esac

# Execute the requested container process
exec "$@"
