#!/usr/bin/env bash
# Starts the TimesTen daemon, makes sure the test database "gotest" exists, then idles.
set -euo pipefail
source "$TIMESTEN_HOME/bin/ttenv.sh" >/dev/null
"$TIMESTEN_HOME/bin/ttdaemonadmin" -start -force >/dev/null || true

ODBC_INI="$TIMESTEN_HOME/conf/sys.odbc.ini"
DS_DIR="$TIMESTEN_HOME/datastore"
mkdir -p "$DS_DIR/logs_gotest"
if ! grep -q '^\[gotest\]' "$ODBC_INI" 2>/dev/null; then
  cat >>"$ODBC_INI" <<EOF

[gotest]
Driver=$TIMESTEN_HOME/install/lib/libtten.so
DataStore=$DS_DIR/gotest
DatabaseCharacterSet=AL32UTF8
ConnectionCharacterSet=AL32UTF8
PermSize=256
TempSize=128
LogBufMB=64
LogDir=$DS_DIR/logs_gotest
AutoCreate=1

[gotestCS]
TTC_SERVER=localhost/6625
TTC_SERVER_DSN=gotest
EOF
fi

if [ ! -f "$DS_DIR/.gotest-ready" ]; then
  ttIsql -v1 -connstr "dsn=gotest" -e "
    create user gotest identified by gotest;
    grant create session, create table, create procedure, create sequence, create view, xla to gotest;
    grant admin to gotest;
    quit" && touch "$DS_DIR/.gotest-ready"
fi
"$TIMESTEN_HOME/bin/ttadmin" -ramPolicy always gotest >/dev/null 2>&1 || true
"$TIMESTEN_HOME/bin/ttadmin" -ramLoad gotest >/dev/null 2>&1 || true
echo "tt-dev: database gotest ready"

if [ "$#" -gt 0 ]; then exec "$@"; fi
trap 'ttIsql -v1 -connstr "dsn=gotest" -e "call ttCkpt; quit" >/dev/null 2>&1; exit 0' TERM INT
while :; do sleep 3600 & wait $!; done
