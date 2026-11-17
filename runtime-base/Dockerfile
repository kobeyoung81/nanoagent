# NanoAgent Runtime — the tool layer every agent container needs.
#
# Build once per toolchain version, on the same Docker daemon the CI uses:
#
#   docker build -t kobeyoung81/nanoagent-runtime:1 nanoagent/runtime-base
#
# The service image (`nanoagent/Dockerfile`) then starts from this image and only
# copies the agent binary, md2pdf, the template and the entrypoint. Two reasons
# it lives here instead of in the service image:
#
#   * officecli is a pinned, self-contained .NET binary whose GitHub release is
#     slow and unreliable from the build host (HTTP/2 PROTOCOL_ERROR, 400s+ of
#     retries). It is copied from the released agent image, which already
#     carries the verified file, so no build reaches GitHub for it.
#   * The apt/nvm/Go layers change far less often than the agent code, so a
#     service build should not reinstall them.
#
# Toolchain versions are pinned below. Bump them deliberately: build :2, point
# nanoagent/Dockerfile at it, keep :1 for rollback.

FROM kobeyoung81/nanoagent:0.1.0 AS officecli-source

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

# officecli comes from the released agent image: pinned version, already
# verified, and no network. The sha256 is still the authority, so a wrong or
# stale source fails the build loudly.
COPY --from=officecli-source /usr/local/bin/officecli /usr/local/bin/officecli
RUN echo "${OFFICECLI_SHA256}  /usr/local/bin/officecli" | sha256sum -c - \
    && chmod +x /usr/local/bin/officecli
