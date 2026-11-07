#!/usr/bin/env bash
# Isolated, synthetic-only database validation. Requires Docker, gzip, python3.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
PG_IMAGE=${RETENTION_PG_IMAGE:-postgres:18.1-alpine3.23}
GO_IMAGE=${RETENTION_GO_IMAGE:-golang:1.27.3-alpine}
PG_NAME="sub2api-retention-$(date +%s)-$$"
GO_NAME="$PG_NAME-go"
DB_NAME=sub2api_retention_test
PASSWORD=$(python3 -c 'import secrets; print(secrets.token_hex(20))')
mkdir -p "$ROOT/output/retention-test"
ARTIFACTS=$(mktemp -d "$ROOT/output/retention-test/tmp-retention.XXXXXXXX")
cleanup() {
  docker rm -f -v "$GO_NAME" >/dev/null 2>&1 || true
  docker rm -f -v "$PG_NAME" >/dev/null 2>&1 || true
}
trap cleanup EXIT
trap 'exit 130' INT TERM
printf 'Artifacts: %s\nPostgreSQL: %s\n' "$ARTIFACTS" "$PG_IMAGE"
# These names are unique to this run. No existing containers or volumes touched.
docker run -d --name "$PG_NAME" --label sub2api.test=retention \
  -e POSTGRES_DB="$DB_NAME" -e POSTGRES_PASSWORD="$PASSWORD" \
  -p 127.0.0.1::5432 "$PG_IMAGE" \
  -c autovacuum=off > "$ARTIFACTS/container-id.txt"
for ((i=0;i<60;i++)); do
  if docker exec "$PG_NAME" pg_isready -U postgres -d "$DB_NAME" >/dev/null 2>&1; then break; fi
  sleep 1
done
docker exec "$PG_NAME" pg_isready -U postgres -d "$DB_NAME"
PORT=$(docker port "$PG_NAME" 5432/tcp | sed 's/.*://')
DSN="postgres://postgres:$PASSWORD@127.0.0.1:$PORT/$DB_NAME?sslmode=disable"
go_test() {
  local logfile=$1 package=$2 pattern=$3
  printf 'Running %s\n' "$logfile"
  if ! docker run --rm --name "$GO_NAME" --network host \
    -e RETENTION_TEST_POSTGRES_DSN="$DSN" \
    -e HTTP_PROXY= -e HTTPS_PROXY= -e ALL_PROXY= \
    -e http_proxy= -e https_proxy= -e all_proxy= \
    -e GOPROXY=https://goproxy.cn,direct \
    -v "$ROOT:/repo:ro" -v sub2api-go-mod:/go/pkg/mod \
    -v sub2api-go-build:/root/.cache/go-build -w /repo/backend \
    "$GO_IMAGE" go test -tags=retentionintegration -count=1 -v -timeout=10m \
    "$package" -run "$pattern" > "$ARTIFACTS/$logfile.log" 2>&1; then
    tail -100 "$ARTIFACTS/$logfile.log"
    return 1
  fi
  tail -3 "$ARTIFACTS/$logfile.log"
}
psql_file() {
  docker exec -i "$PG_NAME" psql -X -v ON_ERROR_STOP=1 -U postgres -d "$DB_NAME" "$@"
}
snapshot_sizes() { psql_file --csv < "$ROOT/tools/retention-test/sizes.sql" > "$ARTIFACTS/$1-sizes.csv"; }
snapshot_data() { psql_file -At < "$ROOT/tools/retention-test/snapshot.sql" > "$ARTIFACTS/$1-snapshot.txt"; }
snapshot_indexes() { psql_file -At < "$ROOT/tools/retention-test/indexes.sql" > "$ARTIFACTS/$1-indexes.txt"; }
dump_db() {
  docker exec "$PG_NAME" pg_dump -U postgres -d "$DB_NAME" --no-owner --no-acl | gzip -6 > "$ARTIFACTS/$1.sql.gz"
  gzip -t "$ARTIFACTS/$1.sql.gz"
}
psql_file -Atc 'SELECT version()' > "$ARTIFACTS/postgres-version.txt"
go_test migrations ./internal/repository '^TestRetentionPostgresMigrations$'
go_test edge-cases ./internal/service '^TestRetentionPostgres(Boundaries|RollbackAndCancellation|Disabled|LeaderLock)$'
psql_file < "$ROOT/tools/retention-test/seed.sql" > "$ARTIFACTS/seed.log"
snapshot_sizes before
snapshot_indexes before
dump_db before
go_test cleanup ./internal/service '^TestRetentionPostgresWorkerDataset$'
go_test billing-replay ./internal/repository '^TestRetentionPostgresBillingReplay$'
snapshot_sizes after-delete
snapshot_indexes after-delete
diff -u "$ARTIFACTS/before-indexes.txt" "$ARTIFACTS/after-delete-indexes.txt"
snapshot_data after-delete
dump_db after
# Autovacuum is disabled ONLY in this disposable container to make this phase
# reproducible. Ordinary VACUUM preserves indexes; no VACUUM FULL or REINDEX.
psql_file -c 'VACUUM (ANALYZE)' > "$ARTIFACTS/vacuum.log"
snapshot_sizes after-vacuum
snapshot_indexes after-vacuum
diff -u "$ARTIFACTS/before-indexes.txt" "$ARTIFACTS/after-vacuum-indexes.txt"
snapshot_data after-vacuum
diff -u "$ARTIFACTS/after-delete-snapshot.txt" "$ARTIFACTS/after-vacuum-snapshot.txt"
# Restore into a new, empty database in the SAME disposable container.
docker exec "$PG_NAME" dropdb -U postgres "$DB_NAME"
docker exec "$PG_NAME" createdb -U postgres "$DB_NAME"
gzip -dc "$ARTIFACTS/after.sql.gz" | psql_file > "$ARTIFACTS/restore.log"
snapshot_data restored
diff -u "$ARTIFACTS/after-delete-snapshot.txt" "$ARTIFACTS/restored-snapshot.txt"
snapshot_sizes restored
go_test restored-migrations ./internal/repository '^TestRetentionPostgresMigrations$'
go_test restored-billing-replay ./internal/repository '^TestRetentionPostgresBillingReplay$'
python3 - "$ARTIFACTS" <<'PY'
import json, pathlib, sys
p = pathlib.Path(sys.argv[1])
before, after = (p.joinpath(name + '.sql.gz').stat().st_size for name in ('before', 'after'))
result = dict(before_gzip_bytes=before, after_gzip_bytes=after,
              reduction_percent=round((1-after/before)*100,2),
              restore_rows_and_indexes_identical=True)
p.joinpath('result.json').write_text(json.dumps(result, indent=2)+'\n')
print(json.dumps(result, indent=2))
PY
printf 'PASS. Logs, dumps, sizes and row/index checksums: %s\n' "$ARTIFACTS"
