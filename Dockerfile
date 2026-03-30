# syntax=docker/dockerfile:1
# ── Stashy Docker Image ────────────────────────────────────────────────────
# Pulls the pre-built binary from the GitHub release and wraps it with ffmpeg.
# Supports: linux/amd64, linux/arm64, linux/arm/v7, linux/arm/v6
# Build args:
#   RELEASE_TAG  — release tag to pull binaries from (default: latest_develop)

FROM debian:bookworm-slim

ARG RELEASE_TAG=latest_develop
ARG TARGETPLATFORM

# ffmpeg  — needed for transcoding and subtitle extraction
# wget    — used to download the binary below
# ca-certificates — needed for HTTPS
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        ffmpeg \
        wget \
        ca-certificates && \
    rm -rf /var/lib/apt/lists/*

# Pick the right binary for the target platform and download it
RUN set -eux; \
    case "$TARGETPLATFORM" in \
        "linux/amd64")  BINARY="stash-linux"        ;; \
        "linux/arm64")  BINARY="stash-linux-arm64v8" ;; \
        "linux/arm/v7") BINARY="stash-linux-arm32v7" ;; \
        "linux/arm/v6") BINARY="stash-linux-arm32v6" ;; \
        *) echo "Unsupported platform: $TARGETPLATFORM" && exit 1 ;; \
    esac; \
    wget -q \
        "https://github.com/driftywinds/stashy/releases/download/${RELEASE_TAG}/${BINARY}" \
        -O /usr/local/bin/stashy; \
    chmod +x /usr/local/bin/stashy

# /config  — config file, database, cache, generated content
# /media   — your media library (mount as many as you need)
VOLUME ["/config", "/media"]

EXPOSE 9999

# STASH_CONFIG_FILE tells stashy where to read/write its config.
# --nobrowser stops it trying to open a browser inside the container.
ENV STASH_CONFIG_FILE=/config/config.yml

ENTRYPOINT ["/usr/local/bin/stashy", "--nobrowser"]