# Worker image for boardingparty runs: a tiny Session pod runs this image.
# tiny injects its own payload (Claude Code, tmux, entrypoint) via an init
# container and overrides the command, so no ENTRYPOINT/CMD is set here.
# Requirements from tiny: glibc, git, non-root (uid 1000) friendly.
FROM mcr.microsoft.com/playwright:v1.64.1-noble

ARG PLAYWRIGHT_MCP_VERSION=0.0.84

RUN apt-get update \
    && apt-get install -y --no-install-recommends git curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Global install; browsers come from the base image (PLAYWRIGHT_BROWSERS_PATH=/ms-playwright).
RUN npm i -g @playwright/mcp@${PLAYWRIGHT_MCP_VERSION} && npm cache clean --force

# @playwright/mcp always tracks a Playwright alpha (1.65.0-alpha), which expects a
# newer Chromium revision (1252) than the base image ships (1248, Playwright 1.64.1).
# Fetch the revision it wants into the same browsers dir; --only-shell style trimming
# is not offered by this command. The base browsers stay as a fallback.
ENV PLAYWRIGHT_BROWSERS_PATH=/ms-playwright
RUN playwright-mcp install-browser chrome-for-testing \
    && chmod -R a+rX /ms-playwright
WORKDIR /workspace
