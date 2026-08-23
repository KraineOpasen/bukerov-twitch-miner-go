# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /build

# Install git for version detection and ca-certificates for HTTPS
# libstdc++ and libgcc are needed for Tailwind CLI v4
RUN apk add --no-cache git ca-certificates tzdata curl xz libstdc++ libgcc

# Download Tailwind CLI v4 (musl version for Alpine, arch-specific)
ARG TARGETARCH
RUN if [ "$TARGETARCH" = "arm64" ]; then \
        curl -sLo /usr/local/bin/tailwindcss https://github.com/tailwindlabs/tailwindcss/releases/download/v4.1.18/tailwindcss-linux-arm64-musl; \
    else \
        curl -sLo /usr/local/bin/tailwindcss https://github.com/tailwindlabs/tailwindcss/releases/download/v4.1.18/tailwindcss-linux-x64-musl; \
    fi \
    && chmod +x /usr/local/bin/tailwindcss

# Download and install UPX (arch-specific)
RUN if [ "$TARGETARCH" = "arm64" ]; then \
        curl -sLo /tmp/upx.tar.xz https://github.com/upx/upx/releases/download/v5.0.2/upx-5.0.2-arm64_linux.tar.xz \
        && cd /tmp && tar -xf upx.tar.xz \
        && mv upx-5.0.2-arm64_linux/upx /usr/local/bin/; \
    else \
        curl -sLo /tmp/upx.tar.xz https://github.com/upx/upx/releases/download/v5.0.2/upx-5.0.2-amd64_linux.tar.xz \
        && cd /tmp && tar -xf upx.tar.xz \
        && mv upx-5.0.2-amd64_linux/upx /usr/local/bin/; \
    fi \
    && rm -rf /tmp/upx*

# Copy go mod files first for better caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build Tailwind CSS
RUN tailwindcss \
    -i internal/web/static/css/input.css \
    -o internal/web/static/css/app.css \
    --minify

# Build arguments for independent version and release-channel injection.
ARG VERSION=dev
ARG CHANNEL=stable

# Build static binary
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath -buildvcs=false \
    -ldflags="-s -w -X github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version.Version=${VERSION} -X github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version.Channel=${CHANNEL}" \
    -o twitch-miner-go \
    ./cmd/miner

# Compress binary with UPX
RUN upx --best --lzma twitch-miner-go

# Final stage - scratch image for minimal size
FROM scratch

# Copy CA certificates for HTTPS requests
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy full timezone database so the TZ env var can select any zone at runtime
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
ENV TZ=UTC

# Copy binary
COPY --from=builder /build/twitch-miner-go /twitch-miner-go

# Create data directories (will be mounted as volumes)
VOLUME ["/config", "/cookies", "/logs", "/database"]

# Working directory
WORKDIR /

# Default config path
ENV CONFIG_PATH=/config/config.json

# The miner binds its dashboard to 127.0.0.1 by default, which inside a
# container would make published ports (-p 5000:5000) unreachable, so the
# image explicitly binds all container interfaces. Actual network exposure is
# then decided by the container runtime's port publishing (docker -p /
# compose ports / the TrueNAS SCALE or unraid app UI). Because this is a
# non-loopback bind, the miner REQUIRES DASHBOARD_USERNAME and
# DASHBOARD_PASSWORD to be set and refuses to start without them; set
# DASHBOARD_INSECURE_NO_AUTH=true instead to explicitly accept an
# unauthenticated dashboard on a trusted network.
ENV DASHBOARD_HOST=0.0.0.0
EXPOSE 5000

# Stable images never consume the repository-wide GitHub Releases update feed.
# Upgrades are operator-controlled changes to an exact stable image tag/digest.
ENV AUTO_UPDATE=false
ENV AUTO_UPDATE_CHECK_INTERVAL=8h

# Exclude this container from Watchtower so an exact stable selection cannot
# be silently replaced.
LABEL com.centurylinklabs.watchtower.enable=false

# Liveness probe: the scratch image has no shell or curl, so the miner binary
# doubles as its own probe (-healthcheck GETs the dashboard's /api/status,
# attaching DASHBOARD_USERNAME/PASSWORD when set; with analytics disabled it
# reports healthy). start-period covers Twitch auth on first boot.
HEALTHCHECK --interval=60s --timeout=10s --start-period=120s --retries=3 \
  CMD ["/twitch-miner-go", "-healthcheck", "-config", "/config/config.json"]

# Run the binary
ENTRYPOINT ["/twitch-miner-go"]
CMD ["-config", "/config/config.json"]
