#!/usr/bin/env bash
# Demo: xla-tail prints INSERT/UPDATE/DELETE of a table as JSON, then deletes its bookmark.
set -u
cd /src
SQL() { ttIsql -v1 -connstr "$TIMESTEN_DSN" -e "$1; quit" >/dev/null; }
SQL "create table tailtest (id number(9) not null primary key, name varchar2(20), amt number(10,2))"
go build -o /tmp/xla-tail ./examples/xla-tail || exit 1
/tmp/xla-tail -bookmark tail_demo -delete gotest.tailtest > /tmp/tail.out 2>/tmp/tail.err &
PID=$!
sleep 2
SQL "insert into tailtest values (1, 'a', 10.5)"
SQL "update tailtest set name = 'b', amt = null where id = 1"
SQL "delete from tailtest"
sleep 3
kill -TERM $PID; wait $PID; echo "xla-tail exit=$?"
cat /tmp/tail.out /tmp/tail.err
SQL "drop table tailtest" && echo "table dropped (bookmark deleted, no subscription left)"
ttIsql -v1 -connstr "$TIMESTEN_DSN" -e "call ttLogHolds; quit" | grep -c XLA | sed 's/^/XLA holds: /'
