# go-timesten — build and test targets.
#
# The library needs the TimesTen headers and libraries at build time (cgo). Locally, everything runs
# inside the dev container (TimesTen + Go + gcc, own database "gotest"):
#
#   make dev-up        build and start the container
#   make test          unit tests            (in the container)
#   make itest         integration tests     (in the container, real database)
#   make race          integration tests with -race and cgocheck
#   make bench         benchmarks
#   make faults        fault injection (restarts the container's TimesTen daemon)
#   make lint          gofmt + go vet
#   make dev-down      stop the container (database volume is kept)
#
# On a host with TimesTen installed set TIMESTEN_HOME and use the *-host targets.

DEV       ?= go-timesten-dev
COMPOSE   ?= docker compose -f dev/compose.yml
EXEC      ?= docker exec $(DEV) bash -lc
PKGS      ?= ./...

.PHONY: dev-up dev-down dev-shell test itest race bench faults lint ci test-host itest-host

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

bench:
	$(EXEC) 'cd /src && go test -tags integration -run XXX -bench . -benchtime 5x ./ttsql/ ./xla/'

faults:
	$(EXEC) 'cd /src && go test -tags "integration faults" -run Survives -count=1 -v ./ttsql/'

lint:
	$(EXEC) 'cd /src && test -z "$$(gofmt -l .)" && go vet -tags "integration faults" $(PKGS)'

ci: lint race

# --- host with TimesTen installed -------------------------------------------------------------
TT_ENV = CGO_ENABLED=1 CGO_CFLAGS=-I$(TIMESTEN_HOME)/install/include CGO_LDFLAGS=-L$(TIMESTEN_HOME)/install/lib \
         LD_LIBRARY_PATH=$(TIMESTEN_HOME)/install/lib LIBPATH=$(TIMESTEN_HOME)/install/lib

test-host:
	$(TT_ENV) go test -count=1 $(PKGS)

itest-host:
	$(TT_ENV) go test -tags integration -count=1 $(PKGS)
