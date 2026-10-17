#!/usr/bin/env bash
# Update a Pogo Pad server to the latest main: back up the data, pull, rebuild
# and restart, then check that it came back up.
#
#   scripts/update.sh [--mode docker|binary] [--force] [--rename-default NAME]
#
# Run it from a clone of the repo on the server. Docker mode (the default when
# docker compose is available) uses the repo's docker-compose.yml. Binary mode
# builds ./pogo-pad and restarts a systemd unit that runs it.
#
# Environment:
#   BACKUP_DIR  where backups go (default: ./backups)
#   KEEP        how many backups to keep (default: 10)
#   HEALTH_URL  checked after the restart (default: http://127.0.0.1:8080/api/v1/health)
#   SERVICE     compose service, or systemd unit in binary mode (default: pogo-pad)
#   POGO_DB     database path in binary mode (default: ./data/pogo-pad.db)
set -euo pipefail

cd "$(dirname "$0")/.."

mode=""
force=0
rename_default=""
while [[ $# -gt 0 ]]; do
	case "$1" in
	--mode) mode="$2"; shift 2 ;;
	--force) force=1; shift ;;
	--rename-default) rename_default="$2"; shift 2 ;;
	-h|--help) sed -n '2,16p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
	*) echo "unknown option: $1" >&2; exit 2 ;;
	esac
done

BACKUP_DIR="${BACKUP_DIR:-./backups}"
KEEP="${KEEP:-10}"
HEALTH_URL="${HEALTH_URL:-http://127.0.0.1:8080/api/v1/health}"
SERVICE="${SERVICE:-pogo-pad}"
POGO_DB="${POGO_DB:-./data/pogo-pad.db}"

log() { printf '\033[1;33m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

if [[ -z "$mode" ]]; then
	if docker compose version >/dev/null 2>&1; then mode=docker; else mode=binary; fi
fi
[[ "$mode" == docker || "$mode" == binary ]] || die "--mode must be docker or binary"

# Run the pogo-pad CLI against the server's database.
pp() {
	if [[ "$mode" == docker ]]; then
		docker compose exec -T "$SERVICE" pogo-pad "$@"
	else
		# Flags must come before the positional arguments: "user rename --db X OLD NEW".
		if [[ "$1" == version ]]; then ./pogo-pad version; else ./pogo-pad "$1" "$2" --db "$POGO_DB" "${@:3}"; fi
	fi
}

# --- 1. Pull ---------------------------------------------------------------

[[ -z "$(git status --porcelain --untracked-files=no)" ]] ||
	die "the working tree has local changes; commit or stash them first"

old_rev="$(git rev-parse HEAD)"
log "Fetching updates"
git fetch --quiet
# The repo can be current while an older build still runs (after a manual
# git pull, say), so compare what is running, not just the commits.
running="$(curl -fsS "$HEALTH_URL" 2>/dev/null | sed -n 's/.*"version":"\([^"]*\)".*/\1/p' || true)"
[[ -n "$running" ]] || running="$(pp version 2>/dev/null || true)"
if [[ "$(git rev-parse HEAD)" == "$(git rev-parse '@{u}')" && $force -eq 0 &&
	"$running" == "$(git describe --tags --always)" ]]; then
	log "Already up to date ($running). Use --force to rebuild anyway."
	if [[ -n "$rename_default" ]]; then
		pp user rename default "$rename_default"
		pp user list
	fi
	exit 0
fi
git merge --ff-only --quiet '@{u}'
version="$(git describe --tags --always)"
log "Updating ${running:-unknown version} -> $version"
git --no-pager log --oneline "$old_rev..HEAD" | sed 's/^/    /'

# --- 2. Build (while the old version keeps running) ------------------------

log "Building $version"
if [[ "$mode" == docker ]]; then
	image="$(docker compose config --images 2>/dev/null | head -n1)"
	image="${image:-pogo-pad:latest}"
	if docker image inspect "$image" >/dev/null 2>&1; then
		docker tag "$image" "${image%:*}:previous"
	fi
	docker compose build --build-arg VERSION="$version" "$SERVICE"
else
	command -v go >/dev/null || die "go is not installed"
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$version" -o pogo-pad.new ./cmd/pogo-pad
fi

# --- 3. Stop and back up ---------------------------------------------------

stamp="$(date +%Y%m%d-%H%M%S)"
backup="$BACKUP_DIR/pogo-pad-$stamp"
mkdir -p "$BACKUP_DIR"

log "Stopping the server"
if [[ "$mode" == docker ]]; then
	docker compose stop "$SERVICE"
	container="$(docker compose ps -aq "$SERVICE")"
	if [[ -n "$container" ]]; then
		log "Backing up /data to $backup"
		# --volumes-from works for both named volumes and bind mounts.
		docker run --rm --volumes-from "$container" -v "$(realpath "$BACKUP_DIR")":/backup alpine \
			sh -c "mkdir /backup/pogo-pad-$stamp && cp -a /data/. /backup/pogo-pad-$stamp/ && chown -R $(id -u):$(id -g) /backup/pogo-pad-$stamp"
	else
		log "No existing container, so there is nothing to back up"
	fi
else
	sudo systemctl stop "$SERVICE"
	if [[ -f "$POGO_DB" ]]; then
		log "Backing up $POGO_DB to $backup"
		mkdir -p "$backup"
		cp -a "$POGO_DB"* "$backup/"
	else
		log "No database at $POGO_DB, so there is nothing to back up"
	fi
fi

# Keep the newest $KEEP backups.
ls -1d "$BACKUP_DIR"/pogo-pad-* 2>/dev/null | sort | head -n -"$KEEP" | xargs -r rm -rf

# --- 4. Start and check ----------------------------------------------------

log "Starting $version"
if [[ "$mode" == docker ]]; then
	docker compose up -d "$SERVICE"
else
	[[ -f pogo-pad ]] && mv pogo-pad pogo-pad.previous
	mv pogo-pad.new pogo-pad
	sudo systemctl start "$SERVICE"
fi

healthy=0
if command -v curl >/dev/null; then
	for _ in $(seq 1 15); do
		if body="$(curl -fsS "$HEALTH_URL" 2>/dev/null)"; then
			healthy=1
			log "Healthy: $body"
			break
		fi
		sleep 1
	done
else
	sleep 3
	pp version >/dev/null && healthy=1
fi

if [[ $healthy -eq 0 ]]; then
	if [[ "$mode" == docker ]]; then
		docker compose logs --tail 30 "$SERVICE" >&2
	else
		sudo journalctl -u "$SERVICE" -n 30 --no-pager >&2
	fi
	echo >&2
	echo "The server did not come back up. To roll back to $(git rev-parse --short "$old_rev"):" >&2
	if [[ "$mode" == docker ]]; then
		cat >&2 <<EOF
  git checkout $old_rev
  docker compose stop $SERVICE
  docker run --rm --volumes-from $container -v "$(realpath "$backup")":/backup alpine \\
    sh -c 'rm -rf /data/* && cp -a /backup/. /data/ && chown -R 65532:65532 /data'
  docker tag ${image%:*}:previous $image
  docker compose up -d $SERVICE
EOF
	else
		cat >&2 <<EOF
  git checkout $old_rev
  sudo systemctl stop $SERVICE
  cp -a $backup/. $(dirname "$POGO_DB")/
  mv pogo-pad.previous pogo-pad
  sudo systemctl start $SERVICE
EOF
	fi
	die "health check failed ($HEALTH_URL)"
fi

# --- 5. Users --------------------------------------------------------------

if [[ -n "$rename_default" ]]; then
	log "Renaming user default -> $rename_default"
	pp user rename default "$rename_default"
fi

log "Users"
pp user list

if pp user list | grep -qw default; then
	cat <<EOF

Notes from before multi-user live under the user "default". To split them up:
  scripts/update.sh --rename-default NAME      # or: pogo-pad user rename default NAME
  pogo-pad user create alice
  pogo-pad token create --user alice --name alice-phone
EOF
fi

log "Done: $version (backup in $backup)"
