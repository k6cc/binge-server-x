# syntax=docker/dockerfile:1.7

# ── Build stage ────────────────────────────────────────────────────
FROM golang:1.26-alpine AS build
WORKDIR /src

# Version is plumbed in via the release workflow's build-arg and
# embedded into the binary via -X main.Version. Local `docker build`
# falls back to "docker" so the binary's logs at least say where it
# came from.
ARG VERSION=docker

# Cache go module downloads layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO disabled keeps modernc.org/sqlite in pure-Go mode (smaller image,
# no glibc dependency in the final stage).
ENV CGO_ENABLED=0
RUN go build -trimpath \
    -ldflags="-s -w -X main.Version=${VERSION}" \
    -o /out/binge-server .

# ── Runtime stage ──────────────────────────────────────────────────
# python:slim (not distroless): the X/Twitter feed shells out to
# gallery-dl, a Python tool. gallery-dl is intentionally UNPINNED — X
# rotates its private GraphQL query-ids periodically and breaks older
# gallery-dl; a plain image rebuild pulls the current release that fixes
# it. (--no-download keeps us to metadata only, so no ffmpeg needed.)
FROM python:3.12-slim
RUN pip install --no-cache-dir gallery-dl
COPY --from=build /out/binge-server /usr/local/bin/binge-server

# Persistent data (SQLite + the generated gallery-dl cookie config).
# Mount a volume here.
VOLUME ["/data"]
ENV BINGE_DB_PATH=/data/binge-server.db

# Default listen addr — overridable. 0.0.0.0 is required because the
# bypass container shares the network namespace; binding to 127.0.0.1
# would not be reachable from other namespaces or the host port-forward.
ENV BINGE_LISTEN_ADDR=0.0.0.0:7878
EXPOSE 7878

ENTRYPOINT ["binge-server"]
