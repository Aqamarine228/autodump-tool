#!/usr/bin/env bash
# Restore every .sql file from OUT_DIR into a scratch database and compare
# CHECKSUM TABLE + SHOW CREATE TABLE with the source tables.
# Usage: tests/e2e/verify.sh OUT_DIR [container]
set -euo pipefail
OUT=$1; C=${2:-autodump-mariadb}; SRC=database_documents; DST=autodump_restore
my() { docker exec -i "$C" mysql --default-character-set=utf8mb4 -uroot -pautodump "$@"; }
my -e "DROP DATABASE IF EXISTS $DST; CREATE DATABASE $DST"
for f in "$OUT"/*.sql; do my "$DST" < "$f"; done
tables=$(my -N -e "SELECT table_name FROM information_schema.tables WHERE table_schema='$DST' ORDER BY table_name")
n=0; bad=0
for t in $tables; do
  a=$(my -N -e "CHECKSUM TABLE $SRC.$t" | awk '{print $2}')
  b=$(my -N -e "CHECKSUM TABLE $DST.$t" | awk '{print $2}')
  da=$(my -N -e "SHOW CREATE TABLE $SRC.$t" | cut -f2-)
  db=$(my -N -e "SHOW CREATE TABLE $DST.$t" | cut -f2-)
  n=$((n+1))
  if [[ "$a" != "$b" || "$da" != "$db" ]]; then bad=$((bad+1)); echo "MISMATCH $t: $a vs $b"; fi
done
echo "verified $n tables, $bad mismatches"
my -e "DROP DATABASE $DST"
[[ $bad -eq 0 ]]
