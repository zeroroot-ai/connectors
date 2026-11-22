# Image of purple-mcp, the read only MCP server of SentinelOne, built from a
# pinned upstream commit. This is not Go code of this repo. See README.md and
# NOTICE in this folder. The build context is the root of the repo.
#
# The upstream server has no authentication of its own. Run this image only
# behind the ToolHive proxy of the connector, which authenticates the daemon.

# Stage 1: fetch the pinned commit, check it, and install the locked
# dependencies into a virtual environment.
FROM ghcr.io/zeroroot-ai/mirror/python:3.13-slim-trixie@sha256:e48f59cdba69a3cbd9ed2cf0f348d90d45a8d350d98134cf1070bc106ee9ed90 AS builder

# The commit of https://github.com/Sentinel-One/purple-mcp (tag v0.7.0). Change
# it only by a reviewed commit that also changes README.md and NOTICE.
ARG PURPLE_MCP_COMMIT=07d4992089b10affff6163f296b1f6cb5734539f
ARG UV_VERSION=0.13.5

RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git \
    && rm -rf /var/lib/apt/lists/* \
    && pip install --no-cache-dir "uv==${UV_VERSION}"

WORKDIR /src
RUN git init -q . \
    && git remote add origin https://github.com/Sentinel-One/purple-mcp.git \
    && git fetch -q --depth 1 origin "${PURPLE_MCP_COMMIT}" \
    && git checkout -q FETCH_HEAD \
    && test "$(git rev-parse HEAD)" = "${PURPLE_MCP_COMMIT}"

WORKDIR /app
RUN cp /src/pyproject.toml /src/uv.lock /src/LICENSE /src/README.md /app/ && cp -r /src/src /app/src
# --locked fails when uv.lock does not match pyproject.toml, so the versions of
# all dependencies are the ones that upstream locked.
RUN uv sync --locked --no-default-groups

# Stage 2: the runtime image, as a non-root user.
FROM ghcr.io/zeroroot-ai/mirror/python:3.13-slim-trixie@sha256:e48f59cdba69a3cbd9ed2cf0f348d90d45a8d350d98134cf1070bc106ee9ed90 AS runtime

ARG PURPLE_MCP_COMMIT=07d4992089b10affff6163f296b1f6cb5734539f
LABEL org.opencontainers.image.title="purple-mcp" \
      org.opencontainers.image.description="Read only MCP server of SentinelOne, built from a pinned upstream commit" \
      org.opencontainers.image.source="https://github.com/Sentinel-One/purple-mcp" \
      org.opencontainers.image.revision="${PURPLE_MCP_COMMIT}" \
      org.opencontainers.image.licenses="MIT"

ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 PYTHONIOENCODING=utf-8

RUN useradd --uid 10001 --no-create-home --shell /usr/sbin/nologin mcp
COPY --from=builder --chown=10001:10001 /app /app
COPY --from=builder /src/LICENSE /licenses/purple-mcp-LICENSE
COPY purple-mcp/NOTICE /licenses/NOTICE

# The server reads PURPLEMCP_CONSOLE_TOKEN and PURPLEMCP_CONSOLE_BASE_URL from
# the environment. The image holds no credential.
WORKDIR /app
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/app/.venv/bin/python", "-u", "-m", "purple_mcp.cli", "--mode", "streamable-http", "--host", "0.0.0.0", "--port", "8080", "--allow-remote-access"]
