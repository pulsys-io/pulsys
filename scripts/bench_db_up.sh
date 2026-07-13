#!/usr/bin/env bash
# bench_db_up.sh -- stand up a local Postgres + seed one API token for the bench.
#
# Pulsys has no open mode: the data plane validates a `Bearer pulsys_...` token
# against Postgres on every request (cushioned by an in-process cache).  So a
# faithful benchmark of the real server must run against a real admin plane.
# This script makes that one command:
#
#   1. ensure a local Postgres is installed + running (AL2023 / dnf),
#   2. run `pulsys-db migrate up` to create the schema,
#   3. ensure a `default` tenant,
#   4. seed ONE robot PAT directly (same SHA-256 hash scheme the admin API uses),
#   5. print, on stdout, exactly two shell-eval lines:
#         export PULSYS_DB_DSN=...
#         export BENCH_PAT=pulsys_...
#
# All progress chatter goes to stderr so the caller can `eval "$(bench_db_up.sh)"`.
#
# Loopback-only, trust auth: the bench host has no inbound security-group rules
# and is ephemeral, so a password adds nothing here.  This is a benchmark
# fixture, not a deployment guide -- production uses the Helm chart + RDS.
set -euo pipefail
export PATH="/usr/bin:/bin:/usr/sbin:/sbin:$PATH"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PGDATA="${PGDATA:-/var/lib/pgsql/data}"
DSN="${BENCH_DB_DSN:-postgres://postgres@127.0.0.1:5432/pulsys?sslmode=disable}"
log() { echo "[bench_db_up] $*" >&2; }

# ----- 1. install Postgres if missing --------------------------------------
# The schema migration needs the citext extension, which ships in the
# -contrib package (not the base server), so install all three together.
need_install=0
command -v initdb >/dev/null 2>&1 || command -v postgresql-setup >/dev/null 2>&1 || need_install=1
rpm -q postgresql15-contrib >/dev/null 2>&1 || need_install=1
if [ "$need_install" = 1 ]; then
	if command -v dnf >/dev/null 2>&1; then
		log "installing postgresql15 + server + contrib (citext) via dnf"
		dnf install -y postgresql15 postgresql15-server postgresql15-contrib >&2
	else
		log "FATAL: postgres/citext missing and no dnf to install it; set BENCH_DB_DSN to an existing database with citext"
		exit 1
	fi
fi
# AL2023 puts pg client/server bins under /usr/bin; some setups use libexec.
export PATH="$PATH:/usr/pgsql-15/bin"

# ----- 2. init cluster + start service -------------------------------------
if [ ! -s "$PGDATA/PG_VERSION" ]; then
	log "initializing cluster at $PGDATA"
	if command -v postgresql-setup >/dev/null 2>&1; then
		postgresql-setup --initdb >&2
	else
		mkdir -p "$PGDATA"; chown postgres:postgres "$PGDATA"
		su - postgres -c "initdb -D '$PGDATA'" >&2
	fi
fi

# Loopback trust auth so the bench (running as root) can connect as postgres.
HBA="$PGDATA/pg_hba.conf"
if [ -f "$HBA" ] && ! grep -q 'bench_db_up' "$HBA"; then
	log "writing loopback trust rules to pg_hba.conf"
	{
		echo "# bench_db_up: loopback trust (benchmark fixture only)"
		echo "local   all all       trust"
		echo "host    all all 127.0.0.1/32 trust"
		echo "host    all all ::1/128      trust"
	} >"$HBA"
fi

# Start only if it isn't already accepting connections (idempotent re-runs).
if ! pg_isready -h 127.0.0.1 -q 2>/dev/null; then
	if command -v systemctl >/dev/null 2>&1 && systemctl enable --now postgresql >/dev/null 2>&1; then
		log "started postgresql via systemd"
	else
		log "starting postgresql via pg_ctl"
		su - postgres -c "pg_ctl -D '$PGDATA' -l '$PGDATA/server.log' -w start" >&2 2>&1 || true
	fi
fi

# ----- 3. wait for readiness -----------------------------------------------
for _ in $(seq 1 30); do
	if pg_isready -h 127.0.0.1 -q 2>/dev/null; then break; fi
	sleep 1
done
pg_isready -h 127.0.0.1 -q || { log "FATAL: postgres did not become ready"; exit 1; }

psql_pg() { psql -h 127.0.0.1 -U postgres -v ON_ERROR_STOP=1 "$@"; }

# ----- 4. database + migrations --------------------------------------------
# Recreate the bench database from scratch each run: it is a disposable fixture,
# and a clean DB avoids golang-migrate "dirty" state from any earlier failure.
log "recreating database 'pulsys'"
psql_pg -tAc "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='pulsys' AND pid<>pg_backend_pid()" >/dev/null 2>&1 || true
dropdb --if-exists -h 127.0.0.1 -U postgres pulsys
createdb -h 127.0.0.1 -U postgres pulsys

# Build pulsys-db if it isn't on PATH (the AMI may not ship it).
PULSYS_DB_BIN="$(command -v pulsys-db || true)"
if [ -z "$PULSYS_DB_BIN" ]; then
	PULSYS_DB_BIN="$ROOT/tmp/bench/bin/pulsys-db"
	if [ ! -x "$PULSYS_DB_BIN" ]; then
		log "building pulsys-db"
		mkdir -p "$ROOT/tmp/bench/bin"
		( cd "$ROOT" && go build -o "$PULSYS_DB_BIN" ./cmd/pulsys-db ) >&2
	fi
fi
log "running migrations"
"$PULSYS_DB_BIN" -dsn "$DSN" migrate up >&2

# ----- 5. ensure tenant -----------------------------------------------------
tenant_id() {
	psql_pg -d pulsys -tAc "SELECT id FROM tenants WHERE name='default' AND deleted_at IS NULL LIMIT 1" | head -n1 | tr -d '[:space:]'
}
TID="$(tenant_id)"
if [ -z "$TID" ]; then
	log "creating tenant 'default'"
	psql_pg -d pulsys -q -c "INSERT INTO tenants(name, display_name) VALUES('default','Default Tenant')" >/dev/null
	TID="$(tenant_id)"
fi
log "tenant id $TID"

# ----- 6. seed one robot PAT (SHA-256 of the full pulsys_<prefix>_<secret>) --
# Matches internal/auth.GeneratePAT: prefix = 8 hex chars, secret = base64url
# of 32 random bytes (no padding), stored hash = sha256(full display string).
PREFIX="$(openssl rand -hex 4)"
SECRET="$(openssl rand -base64 32 | tr '+/' '-_' | tr -d '=')"
DISPLAY="pulsys_${PREFIX}_${SECRET}"
HASH_HEX="$(printf '%s' "$DISPLAY" | sha256sum | awk '{print $1}')"
log "seeding PAT prefix=${PREFIX}"
psql_pg -d pulsys -v ON_ERROR_STOP=1 >/dev/null <<SQL
INSERT INTO tokens(tenant_id, owner_user_id, name, prefix, hash, scopes)
VALUES ('${TID}', NULL, 'bench-harness', '${PREFIX}', decode('${HASH_HEX}','hex'), ARRAY['models:read']::text[]);
SQL

# ----- 7. emit eval lines (stdout ONLY) ------------------------------------
printf 'export PULSYS_DB_DSN=%s\n' "$DSN"
printf 'export BENCH_PAT=%s\n' "$DISPLAY"
log "ready: DSN=$DSN  token prefix=${PREFIX}"
