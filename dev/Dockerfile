# Development / integration-test image: TimesTen Classic (from the xla-data-sync image) + gcc + Go.
# Runs its own TimesTen instance and database — independent of any other container.
#
#   make dev-up      build + start (database "gotest" is created on first start)
#   make itest       run integration tests inside it
ARG TT_IMAGE=xla-data-sync-xla-data-service:latest
ARG GO_IMAGE=golang:1.26

FROM ${GO_IMAGE} AS go

FROM ${TT_IMAGE}
USER root
RUN yum install -y gcc && yum clean all
COPY --from=go /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:$PATH \
    GOPATH=/home/oracle/go \
    GOFLAGS=-buildvcs=false \
    CGO_ENABLED=1 \
    CGO_CFLAGS=-I/usr/tt/instance1/install/include \
    CGO_LDFLAGS=-L/usr/tt/instance1/install/lib \
    LD_LIBRARY_PATH=/usr/tt/instance1/install/lib \
    TIMESTEN_HOME=/usr/tt/instance1 \
    TIMESTEN_DSN="DSN=gotest;UID=gotest;PWD=gotest"
RUN mkdir -p /home/oracle/.cache /home/oracle/go && chown -R oracle:oinstall /home/oracle/.cache /home/oracle/go
COPY --chown=oracle:oinstall --chmod=755 dev/entrypoint.sh /usr/local/bin/tt-dev-entrypoint
USER oracle
WORKDIR /src
ENTRYPOINT ["/usr/local/bin/tt-dev-entrypoint"]
