#!/bin/sh
# Docker entrypoint: drops privileges to PUID/PGID and applies UMASK
# before exec'ing binge-server. Mirrors the linuxserver.io convention so
# unraid / generic Docker templates "just work": the container starts as
# root (so it can rewrite the runtime user's uid/gid), then re-execs the
# daemon as an unprivileged user with the requested umask.
#
# Defaults: PUID=1000, PGID=1000, UMASK=022.
set -e

PUID="${PUID:-1000}"
PGID="${PGID:-1000}"
UMASK="${UMASK:-022}"

USER=app

# Only rewrite the runtime user + chown /data when starting as root.
# `docker run --user 99:100` skips this branch and runs binge-server
# directly as the supplied user (PUID/PGID are then the caller's
# responsibility and just informational).
if [ "$(id -u)" = "0" ]; then
    # Rewrite the static 'app' account's uid/gid to the requested values.
    # -o allows non-unique ids so common unraid values like PUID=99
    # (nobody) / PGID=100 (users) work even if those ids already exist.
    if [ "$(id -g "${USER}")" != "${PGID}" ]; then
        groupmod -o -g "${PGID}" "${USER}"
    fi
    if [ "$(id -u "${USER}")" != "${PUID}" ]; then
        usermod -o -u "${PUID}" -g "${USER}" "${USER}"
    fi

    # Fix ownership of the data volume. Cheap: /data only holds the
    # SQLite DB + cookie config (not media), and a no-op when the volume
    # already belongs to PUID/PGID.
    chown -R "${PUID}:${PGID}" /data 2>/dev/null || true

    # Apply umask BEFORE the privilege drop so it's inherited by the
    # exec'd daemon (and its yt-dlp / gallery-dl subprocesses).
    umask "${UMASK}"
    exec gosu "${USER}" /usr/local/bin/binge-server "$@"
fi

# Non-root entry: just honor UMASK and run as-is.
umask "${UMASK}"
exec /usr/local/bin/binge-server "$@"
