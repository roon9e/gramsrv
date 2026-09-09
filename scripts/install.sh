#!/usr/bin/env bash
set -euo pipefail

umask 077

if ! builtin pwd -P >/dev/null 2>&1; then
  cd /
fi

repo_path=/opt/gramsrv
repo_url=https://github.com/iamxvbaba/gramsrv.git
cdn_url=https://cdn.cloudram.ru

die() {
  printf 'install: %s\n' "$*" >&2
  exit 1
}

prompt_required() {
  local prompt=$1
  local value
  [[ -r /dev/tty ]] || die 'interactive configuration requires a terminal; run with bash or provide a TTY'
  while :; do
    printf '%s: ' "$prompt" >/dev/tty
    IFS= read -r value </dev/tty || die 'input ended while reading configuration'
    printf '\n' >/dev/tty
    [[ -n "$value" ]] && { REPLY=$value; return; }
    printf 'A value is required.\n' >&2
  done
}

prompt_default() {
  local prompt=$1
  local default_value=$2
  local value
  [[ -r /dev/tty ]] || die 'interactive configuration requires a terminal; run with bash or provide a TTY'
  printf '%s [%s]: ' "$prompt" "$default_value" >/dev/tty
  IFS= read -r value </dev/tty || die 'input ended while reading configuration'
  printf '\n' >/dev/tty
  REPLY=${value:-$default_value}
}

set_env_value() {
  local key=$1
  local value=$2
  local escaped_value
  escaped_value=${value//\\/\\\\}
  escaped_value=${escaped_value//&/\\&}
  escaped_value=${escaped_value//|/\\|}

  if grep -qE "^${key}=" "$env_path"; then
    sed -i -E "s|^${key}=.*|${key}=${escaped_value}|" "$env_path"
  else
    printf '\n%s=%s\n' "$key" "$value" >> "$env_path"
  fi
}

env_value() {
  local key=$1
  awk -F= -v key="$key" '$1 == key { print substr($0, length(key) + 2); exit }' "$env_path"
}

prompt_config_value() {
  local key=$1
  local prompt=$2
  local install_default=$3
  local legacy_default=${4:-}
  local current
  current=$(env_value "$key")
  if [[ -n "$current" && "$current" != "$legacy_default" ]]; then
    printf 'Keeping %s=%s\n' "$key" "$current"
    REPLY=$current
  else
    prompt_default "$prompt" "$install_default"
  fi
}

config_default_value() {
  local key=$1
  local prompt=$2
  local install_default=$3
  local legacy_default=${4:-}
  local current
  current=$(env_value "$key")
  if [[ -n "$current" && "$current" != "$legacy_default" ]]; then
    printf 'Keeping %s=%s\n' "$key" "$current"
    REPLY=$current
  else
    prompt_default "$prompt" "$install_default"
  fi
}

download_and_extract() {
  local archive=$1
  local destination=$2
  local archive_path="$download_dir/$archive"

  if [[ -n "$(find "$repo_path/data/$destination" -mindepth 1 -print -quit 2>/dev/null)" ]]; then
    printf 'Skipping %s; %s already contains files.\n' "$archive" "$repo_path/data/$destination"
    return
  fi
  printf 'Downloading %s...\n' "$archive"
  curl --fail --location --retry 3 --silent --show-error \
    --output "$archive_path" "$cdn_url/$archive"
  mkdir -p "$repo_path/data/$destination"
  unzip -q -o "$archive_path" -d "$repo_path/data/$destination"
}

[[ $EUID -eq 0 ]] || die 'run this script as root (for example: sudo bash scripts/install-main-vps.sh)'

prompt_default 'Repository path' "$repo_path"
repo_path=$REPLY
prompt_default 'Repository URL' "$repo_url"
repo_url=$REPLY
prompt_default 'CDN URL' "$cdn_url"
cdn_url=$REPLY
env_path="$repo_path/.env"
compose_source="$repo_path/deploy/docker-compose.yml"

missing_packages=()
command -v git >/dev/null 2>&1 || missing_packages+=(git)
command -v curl >/dev/null 2>&1 || missing_packages+=(curl)
command -v unzip >/dev/null 2>&1 || missing_packages+=(unzip)
command -v ffmpeg >/dev/null 2>&1 || missing_packages+=(ffmpeg)
command -v openssl >/dev/null 2>&1 || missing_packages+=(openssl)
go_missing=false
if [[ -x /usr/local/go/bin/go ]]; then
  export PATH="/usr/local/go/bin:$PATH"
elif ! command -v go >/dev/null 2>&1; then
  go_missing=true
fi
if [[ ${#missing_packages[@]} -gt 0 ]]; then
  command -v apt-get >/dev/null 2>&1 || die "required commands are missing: ${missing_packages[*]}"
  apt-get update
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends ca-certificates openssl "${missing_packages[@]}"
fi
if [[ "$go_missing" = true ]]; then
  [[ "$(uname -m)" = x86_64 ]] || die 'this installer supports official Go for x86_64 only'
  go_version=1.25.0
  go_archive="go${go_version}.linux-amd64.tar.gz"
  go_tmp=$(mktemp -d)
  curl --fail --location --retry 3 --silent --show-error \
    --output "$go_tmp/$go_archive" "https://go.dev/dl/$go_archive"
  rm -rf /usr/local/go
  tar -C /usr/local -xzf "$go_tmp/$go_archive"
  rm -rf "$go_tmp"
  export PATH="/usr/local/go/bin:$PATH"
fi

docker_missing=false
if ! command -v docker >/dev/null 2>&1; then
  docker_missing=true
  printf 'Installing Docker...\n'
  curl -fsSL https://get.docker.com | sh
fi
[[ "$docker_missing" = false ]] || hash -r
docker compose version >/dev/null 2>&1 || die 'Docker Compose is unavailable'

if [[ -e "$repo_path" ]]; then
  [[ -d "$repo_path/.git" ]] || die "$repo_path exists but is not a Git checkout"
  printf 'Updating gramsrv main in %s...\n' "$repo_path"
  git -C "$repo_path" fetch origin main
  git -C "$repo_path" checkout main
  git -C "$repo_path" pull --ff-only origin main
else
  printf 'Cloning gramsrv main into %s...\n' "$repo_path"
  git clone --branch main --single-branch "$repo_url" "$repo_path"
fi
[[ -e "$env_path" ]] || cp "$repo_path/.env.example" "$env_path"
sed -i '/^POSTGRES_PASSWORD=/d' "$env_path"

mkdir -p "$repo_path/data"
if [[ ! -s "$repo_path/data/server_rsa.pem" ]]; then
  openssl genrsa -traditional -out "$repo_path/data/server_rsa.pem" 2048
  chmod 600 "$repo_path/data/server_rsa.pem"
fi
openssl rsa -in "$repo_path/data/server_rsa.pem" -pubout -out "$repo_path/data/server_rsa.pub" >/dev/null 2>&1
chmod 644 "$repo_path/data/server_rsa.pub"

download_dir=$(mktemp -d)
cleanup() { rm -rf "$download_dir"; }
trap cleanup EXIT HUP INT TERM
download_and_extract sticker-seed-v2.zip sticker-seed
download_and_extract official-gifts-v2.zip official-gifts
download_and_extract premium-promo.zip premium-promo

mkdir -p "$repo_path/bin"
printf 'Building gramsrv server...\n'
go -C "$repo_path" build -o "$repo_path/bin/gramsrv" ./cmd/telesrv
printf 'Building gramsrv admin panel...\n'
go -C "$repo_path" build -o "$repo_path/bin/gramsrv-admin" ./cmd/telesrv-admin

printf '\nEnter deployment settings. Values are written to %s.\n\n' "$env_path"

advertise_ip=$(curl --fail --location --silent --show-error https://api.ipify.org) || die 'could not determine the VPS public IP'
printf 'Detected server public/advertised IP: %s\n' "$advertise_ip"
prompt_config_value TELESRV_PUBLIC_BASE_URL 'Public base URL' https://teleram.ru https://telesrv.net; public_base_url=$REPLY
prompt_config_value TELESRV_PUBLIC_WEB_BASE_URL 'Web base URL' https://web.teleram.ru https://weba.telesrv.net; public_web_base_url=$REPLY
config_default_value TELESRV_PUBLIC_APP_SCHEME 'App scheme' owpg telesrv; app_scheme=$REPLY
config_default_value TELESRV_DEFAULT_COUNTRY_CODE 'Country code [RU]' RU CN; country_code=$REPLY
prompt_config_value TELESRV_DEV_AUTH_CODE 'Development auth code' 12345 12345; dev_auth_code=$REPLY

prompt_config_value TELESRV_BRAND_PRODUCT_NAME 'Brand product name' Teleram Telesrv; brand_name=$REPLY
brand_username=${brand_name,,}
desktop_name="$brand_name Desktop"
android_name="$brand_name Android"
ios_name="$brand_name iOS"
macos_name="$brand_name macOS"
web_a_name="$brand_name Web A"
web_k_name="$brand_name Web K"
premium_name="$brand_name Premium"
stars_name="$brand_name Stars"

admin_api_token=$(env_value TELESRV_ADMIN_API_TOKEN)
[[ -n "$admin_api_token" ]] || admin_api_token=$(openssl rand -hex 32)
admin_ui_password=$(env_value TELESRV_ADMIN_UI_PASSWORD)
[[ -n "$admin_ui_password" ]] || admin_ui_password=$(openssl rand -hex 24)
admin_ui_token=$(env_value TELESRV_ADMIN_UI_TOKEN)
[[ -n "$admin_ui_token" ]] || admin_ui_token=$(openssl rand -hex 32)
admin_session_key=$(env_value TELESRV_ADMIN_SESSION_KEY)
[[ -n "$admin_session_key" ]] || admin_session_key=$(openssl rand -hex 32)
postgres_password=$(env_value TELESRV_POSTGRES_PASSWORD)
if [[ "$postgres_password" = telesrv ]]; then
  postgres_password=$(openssl rand -hex 24)
fi
redis_password=$(env_value TELESRV_REDIS_PASSWORD)
[[ -n "$redis_password" ]] || redis_password=$(openssl rand -hex 32)

set_env_value TELESRV_ADVERTISE_IP "$advertise_ip"
set_env_value TELESRV_PUBLIC_BASE_URL "$public_base_url"
set_env_value TELESRV_PUBLIC_WEB_BASE_URL "$public_web_base_url"
set_env_value TELESRV_PUBLIC_APP_SCHEME "$app_scheme"
set_env_value TELESRV_DEFAULT_COUNTRY_CODE "$country_code"
set_env_value TELESRV_DEV_AUTH_CODE "$dev_auth_code"
set_env_value TELESRV_BRAND_PRODUCT_NAME "$brand_name"
set_env_value TELESRV_BRAND_PRODUCT_USERNAME "$brand_username"
set_env_value TELESRV_BRAND_DESKTOP_APP_NAME "$desktop_name"
set_env_value TELESRV_BRAND_ANDROID_APP_NAME "$android_name"
set_env_value TELESRV_BRAND_IOS_APP_NAME "$ios_name"
set_env_value TELESRV_BRAND_MACOS_APP_NAME "$macos_name"
set_env_value TELESRV_BRAND_WEB_A_APP_NAME "$web_a_name"
set_env_value TELESRV_BRAND_WEB_K_APP_NAME "$web_k_name"
set_env_value TELESRV_BRAND_PREMIUM_NAME "$premium_name"
set_env_value TELESRV_BRAND_STARS_NAME "$stars_name"
set_env_value TELESRV_ADMIN_API_TOKEN "$admin_api_token"
set_env_value TELESRV_ADMIN_UI_PASSWORD "$admin_ui_password"
set_env_value TELESRV_ADMIN_UI_TOKEN "$admin_ui_token"
set_env_value TELESRV_ADMIN_SESSION_KEY "$admin_session_key"
set_env_value TELESRV_POSTGRES_PASSWORD "$postgres_password"
set_env_value TELESRV_REDIS_PASSWORD "$redis_password"
set_env_value TELESRV_PREMIUM_GRANT_MONTHS 0
set_env_value TELESRV_STARS_STARTING_GRANT 0
set_env_value TELESRV_STARGIFT_TON_STARTING_GRANT 0
set_env_value TELESRV_UPLOAD_INFLIGHT_MAX_BYTES 52428800
set_env_value TELESRV_UPLOAD_INFLIGHT_MAX_FILES 5

chmod 600 "$env_path"

printf 'Starting PostgreSQL and Redis with Docker Compose...\n'
docker compose --project-directory "$repo_path" --file "$compose_source" --env-file "$env_path" up --detach --wait
escaped_postgres_password=${postgres_password//\'/\'\'}
docker exec -u postgres telesrv-postgres psql -U telesrv -d template1 -v ON_ERROR_STOP=1 -c "ALTER ROLE telesrv PASSWORD '$escaped_postgres_password';"
if ! docker exec telesrv-postgres psql -U telesrv -d template1 -tAc "SELECT 1 FROM pg_database WHERE datname = 'telesrv_main'" | grep -q 1; then
  docker exec telesrv-postgres createdb -U telesrv -d template1 telesrv_main
fi

cat > /etc/systemd/system/gramsrv.service <<EOF
[Unit]
Description=gramsrv Telegram-compatible server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$repo_path
EnvironmentFile=$env_path
ExecStart=$repo_path/bin/gramsrv
Restart=on-failure
RestartSec=5
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF

cat > /etc/systemd/system/gramsrv-admin.service <<EOF
[Unit]
Description=gramsrv admin panel
After=network-online.target gramsrv.service
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$repo_path
EnvironmentFile=$env_path
ExecStart=$repo_path/bin/gramsrv-admin
Restart=on-failure
RestartSec=5
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF

command -v systemctl >/dev/null 2>&1 || die 'systemctl is required to create and start the services'
systemctl daemon-reload
systemctl enable --now gramsrv.service gramsrv-admin.service
systemctl restart gramsrv.service gramsrv-admin.service

printf '\nInstallation and configuration complete.\nRepository: %s\nEnvironment: %s\n' "$repo_path" "$env_path"
journalctl -u gramsrv -f
