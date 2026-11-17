# syntax=docker/dockerfile:1

# A steel instance: steel plus its own Linux desktop and browser, which you
# watch and take over through noVNC on port 6080. Everything that lasts
# (config, secrets, memory, browser profile, the agent's files) lives in
# /home/steel, which compose.yaml puts on a volume.
#
# STEEL_SOURCE=source (the default) builds steel from this checkout.
# STEEL_SOURCE=dist uses dist/steel-linux-<arch>, the binaries a release
# signs, so a release image runs exactly what `steel update` would install.
ARG STEEL_SOURCE=source

FROM --platform=$BUILDPLATFORM golang:1.27.4-trixie AS steel-source
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH VERSION
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X main.version=$VERSION" -o /steel ./cmd/steel

FROM scratch AS steel-dist
ARG TARGETOS TARGETARCH
COPY dist/steel-${TARGETOS}-${TARGETARCH} /steel

FROM steel-${STEEL_SOURCE} AS steel

FROM debian:trixie-slim

# Desktop: Xvfb (a screen with no monitor), openbox, x11vnc and noVNC to
# watch it from a browser, xdotool and scrot for later screen control.
# Browser: Chromium, driven over CDP by Playwright MCP (needs Node).
# Fonts: so pages in any script render as text, not boxes.
# The rest: tools the agent and you expect on a Linux box.
RUN apt-get update && apt-get install -y --no-install-recommends \
      tini ca-certificates \
      xvfb x11vnc openbox novnc websockify xdotool scrot \
      chromium fonts-noto-core fonts-noto-cjk fonts-noto-color-emoji \
      nodejs npm \
      curl git jq less procps python3 \
    && rm -rf /var/lib/apt/lists/*

# Playwright MCP attaches to the desktop's Chromium, so it needs no browser
# of its own.
RUN PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 npm install -g @playwright/mcp@0.0.85 \
    && npm cache clean --force

# Opening the noVNC page connects straight away and scales the desktop to
# the window.
RUN printf '%s\n' '<!doctype html><meta http-equiv="refresh" content="0; url=vnc.html?autoconnect=1&resize=scale">' \
      > /usr/share/novnc/index.html

RUN useradd --create-home --uid 1000 --shell /bin/bash steel \
    && mkdir -p /tmp/.X11-unix && chmod 1777 /tmp/.X11-unix

COPY docker/entrypoint.sh /usr/local/bin/steel-entrypoint
COPY docker/config.json /usr/local/share/steel/config.json
COPY --from=steel /steel /usr/local/bin/steel

# STEEL_CONTAINER makes `steel update` and `steel rollback` refuse: here a
# new version arrives as a new image.
ENV STEEL_CONTAINER=1 DISPLAY=:0 LANG=C.UTF-8
USER steel
WORKDIR /home/steel
EXPOSE 6080

# tini reaps the processes the agent leaves behind and passes SIGTERM to
# steel alone, so the desktop stays up while steel finishes its work.
ENTRYPOINT ["tini", "--", "steel-entrypoint"]
