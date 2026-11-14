# go-timesten — build and test targets.
#
# The library needs the TimesTen headers and libraries at build time (cgo). Locally, everything runs
# inside the dev container (TimesTen + Go + gcc, own database "gotest"):
#
#   make dev-up        build and start the container
#   make test          unit tests            (in the container)
#   make itest         integration tests     (in the container, real database, direct connection)
#   make race          integration tests with -race and cgocheck
#   make itest-cs      integration tests over a client/server connection (same database)
#   make race-cs       the same with -race and cgocheck
#   make netfaults     client/server network failures through an in-test TCP proxy
#   make bench         benchmarks (direct)
#   make bench-cs      benchmarks over client/server
#   make faults        fault injection (restarts the container's TimesTen daemon)
#   make soak          long mixed workload watching process memory (SOAK=30m)
#   make lint          gofmt + go vet
#   make dev-down      stop the container (database volume is kept)
#
# On a host with TimesTen installed set TIMESTEN_HOME and use the *-host targets.

DEV       ?= go-timesten-dev
COMPOSE   ?= docker compose -f dev/compose.yml
EXEC      ?= docker exec $(DEV) bash -lc
PKGS      ?= ./...
SOAK      ?= 30m

# client/server connections to the container's own TimesTen Server (port 6625)
CS_CONN   ?= TTC_SERVER_DSN=gotest;UID=gotest;PWD=gotest
CS_ENV     = TIMESTEN_DSN="TTC_SERVER=localhost/6625;$(CS_CONN)" \
             TIMESTEN_CS_SERVER=localhost:6625 TIMESTEN_CS_CONN="$(CS_CONN)"

.PHONY: dev-up dev-down dev-shell test itest race itest-cs race-cs netfaults bench bench-cs \
	faults soak lint ci test-host itest-host

dev-up:
	$(COMPOSE) up -d --build
	@$(EXEC) 'until ttIsql -v1 -connstr "$$TIMESTEN_DSN" -e "quit" >/dev/null 2>&1; do sleep 1; done; echo ready'

dev-down:
	$(COMPOSE) down

dev-shell:
	docker exec -it $(DEV) bash -l

test:
	$(EXEC) 'cd /src && go test -count=1 $(PKGS)'

itest:
	$(EXEC) 'cd /src && go test -tags integration -count=1 $(PKGS)'

race:
	$(EXEC) 'cd /src && GODEBUG=cgocheck=1 go test -race -tags integration -count=1 $(PKGS)'

# client/server: ttsql only (XLA needs a direct connection)
itest-cs:
	$(EXEC) 'cd /src && $(CS_ENV) go test -tags integration -count=1 ./ttsql/'

race-cs:
	$(EXEC) 'cd /src && $(CS_ENV) GODEBUG=cgocheck=1 go test -race -tags integration -count=1 ./ttsql/'

netfaults:
	$(EXEC) 'cd /src && $(CS_ENV) go test -tags integration -run NetFault -count=1 -v ./ttsql/'

bench:
	$(EXEC) 'cd /src && go test -tags integration -run XXX -bench . -benchtime 5x ./ttsql/ ./xla/'

bench-cs:
	$(EXEC) 'cd /src && $(CS_ENV) go test -tags integration -run XXX -bench . -benchtime 5x ./ttsql/'

faults:
	$(EXEC) 'cd /src && go test -tags "integration faults" -run "Survives|Restart" -count=1 -v ./ttsql/ ./xla/'

soak:
	$(EXEC) 'cd /src && TIMESTEN_SOAK=$(SOAK) go test -tags soak -run Soak -timeout 0 -count=1 -v ./ttsql/'

lint:
	$(EXEC) 'cd /src && test -z "$$(gofmt -l .)" && go vet -tags "integration faults" $(PKGS) && go vet -tags soak ./ttsql/'

ci: lint race race-cs netfaults

# --- host with TimesTen installed -------------------------------------------------------------
TT_ENV = CGO_ENABLED=1 CGO_CFLAGS=-I$(TIMESTEN_HOME)/install/include CGO_LDFLAGS=-L$(TIMESTEN_HOME)/install/lib \
         LD_LIBRARY_PATH=$(TIMESTEN_HOME)/install/lib LIBPATH=$(TIMESTEN_HOME)/install/lib

test-host:
	$(TT_ENV) go test -count=1 $(PKGS)

itest-host:
	$(TT_ENV) go test -tags integration -count=1 $(PKGS)
