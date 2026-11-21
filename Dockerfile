FROM kobeyoung81/base-go:1.27.4-alpine3.24-r2 AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,id=nanosec-gomod,target=/root/go/pkg/mod \
    --mount=type=cache,id=nanosec-gocache,target=/root/.cache/go-build \
    go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
ARG LDFLAGS="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${BUILD_DATE}"

RUN --mount=type=cache,id=nanosec-gomod,target=/root/go/pkg/mod \
    --mount=type=cache,id=nanosec-gocache,target=/root/.cache/go-build \
    go build -trimpath -ldflags "${LDFLAGS}" -o /bin/nanosec ./cmd/nanosec

# Runtime: the NanoAgent Runtime image (Ubuntu LTS + toolchain + officecli +
# md2pdf), built once from runtime-base/Dockerfile and published as
# kobeyoung81/nanoagent-runtime:<tag>. A service build copies the binary, the
# template and the entrypoint — nothing else. See docs/DEPLOYMENT.md §3.0.
#
# The container starts as root so the entrypoint can create/rename the
# per-user Linux user (named after the account display name
# via AGENT_USERNAME, falling back to "nanosec"); it then drops to that user
# with gosu. Terminal sessions run as that user and may sudo (NOPASSWD) to
# install tools themselves. The data root is the user's home (/home/<username>)
# — agents/, sharedspace/ and the profile files live there directly.
FROM kobeyoung81/nanoagent-runtime:24.04-node26.13.1-go1.27.4-r2

# Build metadata. All three come from the tagged commit (the version tag, the
# commit sha, and that commit's time), so two builds of one tag describe the same
# moment. `created` must be RFC 3339; the build script passes the commit time.
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.title="nanoagent" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.source="https://gitee.com/kobeyoung81/nanoagent" \
      org.opencontainers.image.licenses="AGPL-3.0-only"

COPY --from=builder /bin/nanosec /usr/local/bin/nanosec

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
