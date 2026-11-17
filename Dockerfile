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
ARG BUILD_DATE=unknown
ARG LDFLAGS="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${BUILD_DATE}"

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

# Runtime: the NanoAgent Runtime image (Ubuntu LTS + toolchain + officecli).
# It is built once from runtime-base/Dockerfile and reused here, so a service
# build only copies the binaries and template below.
#
# The container starts as root so the entrypoint can create/rename the
# per-user Linux user (named after the account display name
# via AGENT_USERNAME, falling back to "nanosec"); it then drops to that user
# with gosu. Terminal sessions run as that user and may sudo (NOPASSWD) to
# install tools themselves. The data root is the user's home (/home/<username>)
# — agents/, sharedspace/ and the profile files live there directly.
FROM kobeyoung81/nanoagent-runtime:1

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