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
# python:slim (not distroless): we shell out to gallery-dl (X/Instagram)
# and yt-dlp (PornHub). Both UNPINNED — these sites rotate their private
# APIs / query-ids periodically and break older releases; a plain image
# rebuild pulls the current versions that fix it. curl_cffi gives yt-dlp
# the browser TLS impersonation PornHub demands (410s without it); ffmpeg
# is for any HLS-only PornHub download/merge. gosu is the privilege-drop
# helper used by docker-entrypoint.sh to support PUID/PGID.
FROM python:3.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg gosu \
    && rm -rf /var/lib/apt/lists/* \
    && pip install --no-cache-dir gallery-dl yt-dlp curl_cffi \
    && groupadd -g 1000 app \
    && useradd -u 1000 -g 1000 -d /home/app -s /sbin/nologin -M app \
    && mkdir -p /data \
    && chown -R app:app /data
COPY --from=build /out/binge-server /usr/local/bin/binge-server
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

# Persistent data (SQLite + the generated gallery-dl cookie config).
# Mount a volume here.
VOLUME ["/data"]
ENV BINGE_DB_PATH=/data/binge-server.db

# Default listen addr — overridable. 0.0.0.0 is required because the
# bypass container shares the network namespace; binding to 127.0.0.1
# would not be reachable from other namespaces or the host port-forward.
ENV BINGE_LISTEN_ADDR=0.0.0.0:7878

# Privilege/permission knobs consumed by docker-entrypoint.sh. Mirrors
# the linuxserver.io convention so unraid templates "just work": the
# container starts as root, rewrites the static `app` account to the
# requested uid/gid, applies the umask, then re-execs the daemon via
# gosu. UMASK=002 + the saver's MkdirAll(0777) yields drwxrwxr-x and
# -rw-rw-r--, owned by PUID:PGID — the standard unraid setup.
ENV PUID=1000
ENV PGID=1000
ENV UMASK=022

EXPOSE 7878

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
