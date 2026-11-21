# NanoAgent Runtime — the tool layer every agent container needs.
#
# Build once per toolchain version, on the same Docker daemon the CI uses:
#
#   docker build -t kobeyoung81/nanoagent-runtime:24.04-node26.13.1-go1.27.4-r3 \
#     nanoagent/runtime-base
#
# The service image (`nanoagent/Dockerfile`) then starts from this image and only
# copies the agent binary, the template and the entrypoint, so a service build
# is a few COPY instructions instead of a toolchain install. md2pdf is built here
# too (it is a pinned clone, not this repository's code), which keeps the git
# clone and the go build out of every agent rebuild.
#
# The tag carries the toolchain versions on purpose: it is immutable, and a
# reader of the service Dockerfile can see what it runs on. Bump the versions,
# bump the revision suffix (-r3), build, point nanoagent/Dockerfile at the new
# tag, and keep the previous tag for rollback.
#
# The image is published to Docker Hub (public, like the services) so a fresh
# machine does not have to rebuild it; it is also kept locally, which is what the
# CI hits first.

FROM kobeyoung81/base-go:1.27.4-alpine3.24-r3 AS md2pdf-builder

ARG MD2PDF_REF=v2.3.0
RUN set -eux; \
    apk add --no-cache git; \
    git clone --depth 1 --branch "${MD2PDF_REF}" --recursive --shallow-submodules \
      https://gitee.com/kobeyoung81/md2pdf.git /src/md2pdf; \
    cd /src/md2pdf; \
    go build -trimpath -o /out/md2pdf ./cmd/md2pdf

# officecli is a pinned, self-contained .NET binary whose GitHub release is slow
# and unreliable from the build host. It is copied from the released agent image,
# which already carries the verified file, so no build reaches GitHub for it.
# The sha256 below is still the authority: a wrong or stale source fails loudly.
FROM kobeyoung81/nanoagent:0.1.0 AS officecli-source

FROM ubuntu:24.04

ARG OFFICECLI_VERSION=v1.0.147
ARG OFFICECLI_SHA256=e8bfe04f670139f526fe4e81f11acc1bc8629e421a20c5ba7a6e25f7a54a31f7
# Toolchain versions (pinned for reproducible images). Bump deliberately, in
# docker/base/versions.env for the build images and here for the agent runtime.
ARG APT_MIRROR=https://mirrors.bfsu.edu.cn
ARG NVM_VERSION=v0.40.7
# SHA-256 of nvm's install.sh at that tag. Both mirrors serve byte-identical
# content, so one digest covers either source.
ARG NVM_INSTALL_SHA256=066ce4eaf4d78eaa6410433bc9ba58faaba646157cbbed6109153e6c24c5f8a5
ARG NODE_VERSION=26.13.1
ARG GO_VERSION=1.27.4
ARG GO_SHA256=7226736c2339719cda28fbf3a54d71aa63bcefe376e98cef00034268dab24840
ARG OPENCODE_VERSION=1.18.29
# nodejs.org is slow and occasionally unreachable from the build host; npmmirror
# serves the same tarballs. The sha256 below is the official one (SHASUMS256.txt
# for v26.13.1), so the mirror cannot substitute a different archive.
ARG NODE_MIRROR=https://npmmirror.com/mirrors/node
ARG NODE_SHA256=5c063e46bab0fbc1c17cc445d7ff129cbdcd0471be6ed83b6ab864f8143bc864
ARG MD2PDF_REF=v2.3.0

LABEL org.opencontainers.image.title="nanoagent-runtime" \
      org.opencontainers.image.version="24.04-node${NODE_VERSION}-go${GO_VERSION}-r3" \
      org.opencontainers.image.revision="${MD2PDF_REF}" \
      org.opencontainers.image.source="https://gitee.com/kobeyoung81/nanoagent" \
      org.opencontainers.image.licenses="AGPL-3.0-only"

# Small, universally useful CLI tools. The agent works through a shell, so
# missing basics are a capability gap rather than a preference: jq for the JSON
# every API returns, wget where curl is the wrong shape, unzip/zip for archives,
# less to page output, nano for a quick edit, procps for ps/top, file/tree/xxd
# for looking at things. All are small and low-surface.
#
# Deliberately NOT included: gcc/g++/make (hundreds of MB and a lot of surface).
# A skill that needs to compile native code should say so and install
# build-essential itself. Override this list to trim or extend per deployment.
#
# libatomic1 is here because Node 26's prebuilt binaries link it (the official
# node image installs it explicitly); Ubuntu 24.04 does not ship it, and without
# it every `node` call dies with "libatomic.so.1: cannot open shared object file".
ARG EXTRA_TOOLS="jq wget unzip zip less nano procps file tree xxd libatomic1"

ENV LANG=C.UTF-8 \
    NVM_DIR=/usr/local/nvm \
    GOPROXY=https://goproxy.cn,direct \
    GO111MODULE=on

# nvm.sh is bash-only; run RUN steps under bash.
SHELL ["/bin/bash", "-c"]

# Base tools: bash/curl/tar/git/sudo are the shell + web-fetch tool-chain the
# agent executes; libicu/libssl/libstdc++/libgcc/zlib/krb5 are the .NET runtime
# dependencies officecli expects; fontconfig + fonts back md2pdf's font
# discovery; python3 enables stdio MCP servers launched via uvx; openssh-client
# lets the agent reach remote hosts; gosu lets the entrypoint drop from root to
# the per-user runtime identity.
#
# Every install and its cleanup live in one RUN: the apt lists, the downloaded
# .debs, the man pages and the docs never reach a committed layer. `man` is not
# installed, and no runtime step reads /usr/share/doc, so removing them costs
# nothing but a few hundred MB of image.
RUN set -eux; \
    export DEBIAN_FRONTEND=noninteractive; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates; \
    for f in /etc/apt/sources.list /etc/apt/sources.list.d/ubuntu.sources; do \
      if [ -f "$f" ]; then \
        sed -i -E "s@https?://(archive|security)\.ubuntu\.com@${APT_MIRROR}@g" "$f"; \
      fi; \
    done; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        ca-certificates tzdata bash curl tar git sudo gosu xz-utils \
        libicu74 libssl3t64 libstdc++6 libgcc-s1 zlib1g libkrb5-3 \
        fontconfig fonts-dejavu-core \
        openssh-client \
        python3 python3-pip python3-venv \
        ${EXTRA_TOOLS}; \
    apt-get clean; \
    rm -rf /var/lib/apt/lists/* /var/cache/apt/archives/*.deb \
           /usr/share/doc/* /usr/share/man/* /usr/share/info/* /tmp/*

# NVM + Node.js LTS installed system-wide (/usr/local/nvm) so they are part of
# the image and available to every runtime shell/user without a per-home
# install. node/npm/npx/opencode are symlinked into /usr/local/bin so non-login
# processes (the nanosec agent, MCP stdio servers launched via npx, a tool that
# shells out to opencode) resolve them too — the nvm bin directory itself is not
# on a non-login PATH. opencode is installed globally via the mirrored registry.
#
# The tarball comes from the mirror and its sha256 is pinned here rather than
# fetched as SHASUMS256.txt: `nvm install` reaches nodejs.org for the tarball and
# iojs.org for the version list, and both are slow or blocked from this build
# host (one attempt spent five minutes on them). Unpacking into nvm's own
# versions directory keeps `nvm use`/`nvm ls` working — only the download moved.
RUN set -eux; \
    mkdir -p "$NVM_DIR"; \
    NVM_SCRIPT=/tmp/nvm-install.sh; \
    curl -fsSL --retry 3 --retry-delay 3 --retry-all-errors "https://raw.githubusercontent.com/nvm-sh/nvm/${NVM_VERSION}/install.sh" -o "$NVM_SCRIPT" \
      || curl -fsSL --retry 3 --retry-delay 3 --retry-all-errors "https://cdn.jsdelivr.net/gh/nvm-sh/nvm@${NVM_VERSION}/install.sh" -o "$NVM_SCRIPT"; \
    echo "${NVM_INSTALL_SHA256}  ${NVM_SCRIPT}" | sha256sum -c -; \
    bash "$NVM_SCRIPT"; \
    rm -f "$NVM_SCRIPT"; \
    NODE_PREFIX="$NVM_DIR/versions/node/v${NODE_VERSION}"; \
    mkdir -p "$NODE_PREFIX"; \
    curl -fL --retry 3 --retry-delay 3 -o /tmp/node.tar.xz \
      "${NODE_MIRROR}/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-x64.tar.xz"; \
    echo "${NODE_SHA256}  /tmp/node.tar.xz" | sha256sum -c -; \
    tar -xJf /tmp/node.tar.xz -C "$NODE_PREFIX" --strip-components=1; \
    rm -f /tmp/node.tar.xz; \
    mkdir -p "$NVM_DIR/alias"; \
    printf 'v%s\n' "${NODE_VERSION}" > "$NVM_DIR/alias/default"; \
    NODE_BIN="$NODE_PREFIX/bin"; \
    NPM_GLOBAL_CONFIG="$NODE_PREFIX/etc/npmrc"; \
    mkdir -p "$(dirname "$NPM_GLOBAL_CONFIG")"; \
    printf 'registry=https://registry.npmmirror.com\n' > "$NPM_GLOBAL_CONFIG"; \
    ln -sf "$NODE_BIN/node" /usr/local/bin/node; \
    ln -sf "$NODE_BIN/npm" /usr/local/bin/npm; \
    ln -sf "$NODE_BIN/npx" /usr/local/bin/npx; \
    npm install -g --allow-scripts=opencode-ai "opencode-ai@${OPENCODE_VERSION}"; \
    ln -sf "$NODE_BIN/opencode" /usr/local/bin/opencode; \
    npm cache clean --force; \
    rm -rf "$NVM_DIR/.cache" "$NVM_DIR/.git" /root/.npm /tmp/*; \
    printf 'export NVM_DIR=/usr/local/nvm\n[ -s "$NVM_DIR/nvm.sh" ] && . "$NVM_DIR/nvm.sh"\n' > /etc/profile.d/nvm.sh; \
    printf '\n# NVM (system-wide)\nexport NVM_DIR=/usr/local/nvm\n[ -s "$NVM_DIR/nvm.sh" ] && . "$NVM_DIR/nvm.sh"\n' >> /etc/bash.bashrc

# Go from the Alibaba Cloud mirror (official go.dev is slow from the build
# host); the archive checksum is verified before extraction. Go lands in
# /usr/local/go and is on PATH for the agent process and all shells.
#
# /usr/local/go/{doc,test,api,misc} are not needed to build, test or document
# code (all of them are derived from the source tree), so they are removed in
# the same layer.
RUN set -eux; \
    curl -fL -o /tmp/go.tar.gz "https://mirrors.aliyun.com/golang/go${GO_VERSION}.linux-amd64.tar.gz"; \
    echo "${GO_SHA256}  /tmp/go.tar.gz" | sha256sum -c -; \
    tar -C /usr/local -xzf /tmp/go.tar.gz; \
    rm -f /tmp/go.tar.gz; \
    rm -rf /usr/local/go/doc /usr/local/go/test /usr/local/go/api /usr/local/go/misc; \
    printf 'export PATH=$PATH:/usr/local/go/bin\n' > /etc/profile.d/go.sh

ENV PATH=/usr/local/go/bin:$PATH

COPY --from=officecli-source /usr/local/bin/officecli /usr/local/bin/officecli
RUN echo "${OFFICECLI_SHA256}  /usr/local/bin/officecli" | sha256sum -c - \
    && chmod +x /usr/local/bin/officecli

# md2pdf and its data. Built above from a pinned tag, so the version here and the
# version in the image cannot drift apart.
COPY --from=md2pdf-builder /out/md2pdf /usr/local/bin/md2pdf
COPY --from=md2pdf-builder /src/md2pdf/highlight/syntax_files /usr/share/md2pdf/syntax_files
# CJK-capable font for default automatic font discovery (/usr/share/fonts/...).
COPY --from=md2pdf-builder /src/md2pdf/testdata/fonts/gbsn00lp.ttf /usr/share/fonts/md2pdf/ArphicGB.ttf
