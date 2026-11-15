# Development / integration-test image: TimesTen Classic + gcc + Go.
# Runs its own TimesTen instance and database — independent of any other container.
#
# TT_IMAGE must be an image you build or pull yourself (TimesTen is not redistributable): an
# Oracle Linux based image with TimesTen 22.1 Classic installed as instance /usr/tt/instance1, owned
# by user oracle (group oinstall), with yum available. See CONTRIBUTING.md.
#
#   TT_IMAGE=my-timesten:22.1 make dev-up   build + start (database "gotest" is created on first start)
#   make itest                              run integration tests inside it
ARG TT_IMAGE
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
