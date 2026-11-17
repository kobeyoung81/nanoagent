FROM golang:1.24-alpine AS builder

WORKDIR /src
RUN go env -w GO111MODULE=on
RUN go env -w GOPROXY=https://goproxy.cn,direct
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
ARG LDFLAGS="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}"

RUN CGO_ENABLED=0 go build -trimpath -ldflags "${LDFLAGS}" -o /bin/nanosec ./cmd/nanosec

# md2pdf is built from the maintained fork (gitee.com/kobeyoung81/md2pdf, v2
# module). It needs go >= 1.25 (one major release ahead of nanosec's own
# builder), so it gets its own stage. The gohighlight syntax files ship as a
# git submodule; --recursive pulls them, and they are copied into the runtime
# image at /usr/share/md2pdf/syntax_files for out-of-the-box highlighting.
FROM golang:1.25-alpine AS md2pdf-builder

ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}
ARG MD2PDF_REF=v2.3.0

RUN apk add --no-cache git
RUN git clone --depth 1 --branch "${MD2PDF_REF}" --recursive --shallow-submodules \
      https://gitee.com/kobeyoung81/md2pdf.git /src/md2pdf
WORKDIR /src/md2pdf
RUN CGO_ENABLED=0 go build -trimpath -o /out/md2pdf ./cmd/md2pdf

# Runtime: Ubuntu LTS. The container starts as root so the entrypoint can
# create/rename the per-user Linux user (named after the account display name
# via AGENT_USERNAME, falling back to "nanosec"); it then drops to that user
# with gosu. Terminal sessions run as that user and may sudo (NOPASSWD) to
# install tools themselves. The data root is the user's home (/home/<username>)
# — agents/, sharedspace/ and the profile files live there directly.
FROM ubuntu:24.04

ARG OFFICECLI_VERSION=v1.0.147
# SHA-256 of the release asset for the version above. Bump both together: a
# mismatch fails the build loudly, which is the point.
ARG OFFICECLI_SHA256=e8bfe04f670139f526fe4e81f11acc1bc8629e421a20c5ba7a6e25f7a54a31f7
# Toolchain versions (pinned for reproducible images). Bump deliberately.
ARG APT_MIRROR=https://mirrors.bfsu.edu.cn
ARG NVM_VERSION=v0.40.7
# SHA-256 of nvm's install.sh at that tag. Both mirrors serve byte-identical
# content, so one digest covers either source.
ARG NVM_INSTALL_SHA256=066ce4eaf4d78eaa6410433bc9ba58faaba646157cbbed6109153e6c24c5f8a5
ARG NODE_VERSION=24.20.0
ARG GO_VERSION=1.27.1
ARG GO_SHA256=63d339f0da5ab53635a56f2490a7984dfe12dfcff22ad749f63edaf590168445
ARG OPENCODE_VERSION=1.18.29

# Small, universally useful CLI tools. The agent works through a shell, so
# missing basics are a capability gap rather than a preference: jq for the JSON
# every API returns, wget where curl is the wrong shape, unzip/zip for archives,
# less to page output, nano for a quick edit, procps for ps/top, file/tree/xxd
# for looking at things. All are small and low-surface.
#
# Deliberately NOT included: gcc/g++/make (hundreds of MB and a lot of surface).
# A skill that needs to compile native code should say so and install
# build-essential itself. Override this list to trim or extend per deployment.
ARG EXTRA_TOOLS="jq wget unzip zip less nano procps file tree xxd"

# GOPROXY/GO111MODULE stay environment variables deliberately: `go` reads them
# from the environment for every process and every user, whereas its config file
# is per-user ($HOME/.config/go/env) and would only cover whichever user happened
# to exist at build time. npm's registry, by contrast, moves to the global
# npmrc written with the toolchain below — an env var would shadow
# `npm config set registry`, so an agent could not change it.
ENV LANG=C.UTF-8 \
    NVM_DIR=/usr/local/nvm \
    GOPROXY=https://goproxy.cn,direct \
    GO111MODULE=on

# nvm.sh is bash-only; run RUN steps under bash.
SHELL ["/bin/bash", "-c"]

# Toolchain is baked in before any source COPY so frequent nanosec rebuilds
# reuse these cached layers instead of reinstalling tools in every container.
#
# Switch APT to the BFSU mirror first: the build host runs on a China network
# where archive.ubuntu.com is slow/unreachable. Only the host is replaced; the
# /ubuntu/ path and components are unchanged. Both the deb822 (ubuntu.sources)
# and legacy (sources.list) formats are handled.
#
# Base tools: bash/curl/tar/git/sudo are the shell + web-fetch tool-chain the
# agent executes; libicu/libssl/libstdc++/libgcc/zlib/krb5 are the .NET runtime
# dependencies officecli expects; fontconfig + fonts back md2pdf's font
# discovery; python3 enables stdio MCP servers launched via uvx; openssh-client
# lets the agent reach remote hosts; gosu lets the entrypoint drop from root to
# the per-user runtime identity.
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
    rm -rf /var/lib/apt/lists/*

# NVM + Node.js LTS installed system-wide (/usr/local/nvm) so they are part of
# the image and available to every runtime shell/user without a per-home
# install. node/npm/npx/opencode are symlinked into /usr/local/bin so non-login
# processes (the nanosec agent, MCP stdio servers launched via npx, a tool that
# shells out to opencode) resolve them too — the nvm bin directory itself is not
# on a non-login PATH. opencode is installed globally via the mirrored registry.
RUN set -eux; \
    mkdir -p "$NVM_DIR"; \
    NVM_SCRIPT=/tmp/nvm-install.sh; \
    curl -fsSL --retry 3 --retry-delay 3 --retry-all-errors "https://raw.githubusercontent.com/nvm-sh/nvm/${NVM_VERSION}/install.sh" -o "$NVM_SCRIPT" \
      || curl -fsSL --retry 3 --retry-delay 3 --retry-all-errors "https://cdn.jsdelivr.net/gh/nvm-sh/nvm@${NVM_VERSION}/install.sh" -o "$NVM_SCRIPT"; \
    echo "${NVM_INSTALL_SHA256}  ${NVM_SCRIPT}" | sha256sum -c -; \
    bash "$NVM_SCRIPT"; \
    rm -f "$NVM_SCRIPT"; \
    . "$NVM_DIR/nvm.sh"; \
    nvm install "${NODE_VERSION}"; \
    nvm alias default "${NODE_VERSION}"; \
    NODE_BIN="$NVM_DIR/versions/node/$(nvm version default)/bin"; \
    NPM_GLOBAL_CONFIG="$NVM_DIR/versions/node/$(nvm version default)/etc/npmrc"; \
    mkdir -p "$(dirname "$NPM_GLOBAL_CONFIG")"; \
    printf 'registry=https://registry.npmmirror.com\n' > "$NPM_GLOBAL_CONFIG"; \
    ln -sf "$NODE_BIN/node" /usr/local/bin/node; \
    ln -sf "$NODE_BIN/npm" /usr/local/bin/npm; \
    ln -sf "$NODE_BIN/npx" /usr/local/bin/npx; \
    npm install -g --allow-scripts=opencode-ai "opencode-ai@${OPENCODE_VERSION}"; \
    ln -sf "$NODE_BIN/opencode" /usr/local/bin/opencode; \
    printf 'export NVM_DIR=/usr/local/nvm\n[ -s "$NVM_DIR/nvm.sh" ] && . "$NVM_DIR/nvm.sh"\n' > /etc/profile.d/nvm.sh; \
    printf '\n# NVM (system-wide)\nexport NVM_DIR=/usr/local/nvm\n[ -s "$NVM_DIR/nvm.sh" ] && . "$NVM_DIR/nvm.sh"\n' >> /etc/bash.bashrc

# Go from the Alibaba Cloud mirror (official go.dev is slow from the build
# host); the archive checksum is verified before extraction. Go lands in
# /usr/local/go and is on PATH for the agent process and all shells.
RUN set -eux; \
    curl -fL -o /tmp/go.tar.gz "https://mirrors.aliyun.com/golang/go${GO_VERSION}.linux-amd64.tar.gz"; \
    echo "${GO_SHA256}  /tmp/go.tar.gz" | sha256sum -c -; \
    tar -C /usr/local -xzf /tmp/go.tar.gz; \
    rm /tmp/go.tar.gz; \
    printf 'export PATH=$PATH:/usr/local/go/bin\n' > /etc/profile.d/go.sh

ENV PATH=/usr/local/go/bin:$PATH

RUN curl -fsSL --retry 5 --retry-delay 3 --retry-all-errors -o /usr/local/bin/officecli "https://github.com/iOfficeAI/OfficeCLI/releases/download/${OFFICECLI_VERSION}/officecli-linux-x64" \
    && echo "${OFFICECLI_SHA256}  /usr/local/bin/officecli" | sha256sum -c - \
    && chmod +x /usr/local/bin/officecli

COPY --from=builder /bin/nanosec /usr/local/bin/nanosec
COPY --from=md2pdf-builder /out/md2pdf /usr/local/bin/md2pdf
COPY --from=md2pdf-builder /src/md2pdf/highlight/syntax_files /usr/share/md2pdf/syntax_files
# CJK-capable font for default automatic font discovery (/usr/share/fonts/...).
COPY --from=md2pdf-builder /src/md2pdf/testdata/fonts/gbsn00lp.ttf /usr/share/fonts/md2pdf/ArphicGB.ttf

# Image template used to seed per-agent homes.
COPY template/ /usr/share/nanosec/template/

# Standard dircolors database. ubuntu:24.04's coreutils ships an EMPTY
# compiled-in database for `dircolors -b`, so the skel ~/.bashrc would set
# LS_COLORS='' and disable ls coloring. Seeding ~/.dircolors from this file
# (the skel .bashrc prefers it) restores directory/link/file colors.
COPY runtime/dircolors /usr/share/nanosec/dircolors

# The per-user runtime identity is created at container start by the entrypoint
# (ubuntu:24.04 ships a default 'ubuntu' user at uid 1000 which gets renamed or
# replaced); the data root /home/<username> is owned by that user.
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

WORKDIR /

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]

# Liveness probe. The entrypoint execs the hypervisor as PID 1, so a dead agent
# already takes the container down; what this catches is the weaker case — a
# runtime that is present but broken (corrupt binary, missing shared library) —
# so `docker ps` shows the container as unhealthy instead of cheerfully "Up".
HEALTHCHECK --interval=60s --timeout=10s --start-period=15s --retries=3 \
    CMD ["/usr/local/bin/nanosec", "version"]