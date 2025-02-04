FROM golang:1.16.2 AS builder
ENV APP_ROOT=/opt/app-root
ENV GOPATH=$APP_ROOT

WORKDIR $APP_ROOT/src/
ADD go.mod go.sum $APP_ROOT/src/
RUN go mod download

# Add source code
ADD ./ $APP_ROOT/src/

RUN go build ./cmd/rekor-server
RUN CGO_ENABLED=0 go build -gcflags "all=-N -l" -o rekor-server_debug ./cmd/rekor-server

# Multi-Stage production build
FROM golang:1.16.2 as deploy

# Retrieve the binary from the previous stage
COPY --from=builder /opt/app-root/src/rekor-server /usr/local/bin/rekor-server

# Set the binary as the entrypoint of the container
CMD ["rekor-server", "serve"]

# debug compile options & debugger
FROM deploy as debug
RUN go get github.com/go-delve/delve/cmd/dlv

# overwrite server and include debugger
COPY --from=builder /opt/app-root/src/rekor-server_debug /usr/local/bin/rekor-server
