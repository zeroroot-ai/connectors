# purple-mcp

An image of purple-mcp, the MCP server that SentinelOne publishes for its console. SentinelOne gives no image, so this folder builds one from a pinned upstream commit. It holds no Go code. The image workflow of the repo finds the `Dockerfile` and builds and signs the image like the other servers.

SentinelOne reads (alerts, vulnerabilities, misconfigurations, the asset inventory, Purple AI) use this server. The response actions (disconnect and reconnect of an agent) stay in the `sentinelone` server of this repo.

## Upstream

| Item | Value |
|---|---|
| Repository | https://github.com/Sentinel-One/purple-mcp |
| Commit | `07d4992089b10affff6163f296b1f6cb5734539f` (the tag `v0.7.0`, 2026-06-26) |
| License | MIT, copyright 2025 SentinelOne, Inc. The text is in the image at `/licenses/purple-mcp-LICENSE`. See `NOTICE`. |
| Language | Python 3.11 or newer. The image uses 3.13. |

The `Dockerfile` fetches that one commit and stops when the hash of the checkout is not the one that it names. It runs `uv sync --locked`, so the dependencies are the ones that the file `uv.lock` of that commit locks. The base image is the Zero Root mirror of `python:3.13-slim-trixie`, pinned by digest. `uv` has a pinned version. To move to a new upstream version, change `PURPLE_MCP_COMMIT` in both stages, and change this README and `NOTICE` in the same commit.

## Run it only behind the ToolHive proxy

purple-mcp has no authentication of its own. Its README says so: a server that is open on a network gives anyone access to the SentinelOne account of its token. Only the `/health`, `/ready` and `/ping` paths skip authentication, and they give no data.

The image serves streamable HTTP on `:8080` at `/mcp`. It starts with `--allow-remote-access`, because it must listen on the pod address.

1. Run the image only as the server of a connector, behind the ToolHive proxy. The proxy authenticates the daemon.
2. Apply a NetworkPolicy that admits ingress to port 8080 only from the ToolHive proxy pod of the connector.
3. Do not expose the port through a Service of type `LoadBalancer` or `NodePort`, an Ingress or a port-forward.

## Environment

| Variable | Meaning |
|---|---|
| `PURPLEMCP_CONSOLE_TOKEN` | Required. A service user token or a console API token of SentinelOne. Give it the Account or Site scope, not Global. |
| `PURPLEMCP_CONSOLE_BASE_URL` | Required. The address of the console, for example `https://usea1-example.sentinelone.net`. |
| `PURPLEMCP_VT_API_KEY` | Optional. A VirusTotal key. Only the `threat_intel_*` tools use it. They send data to VirusTotal, so leave it out for a buyer with no other egress. |
| `PURPLEMCP_STATELESS_HTTP` | Optional. Set it to `true` for a session-free mode. |

The image holds no credential. Put the token in the secret store of the connector. The names above are from the upstream README. See it for the other settings.

## Tools

The upstream README states that the service is read only. It lists 30 tools: `purple_ai`, `powerquery`, `get_alert`, `list_alerts`, `search_alerts`, `get_alert_notes`, `get_alert_history`, the same set for vulnerabilities and for misconfigurations, `get_inventory_item`, `list_inventory_items`, `search_inventory_items`, `cve_search_by_id`, `cve_search_by_vendor`, `cve_database_status`, and the `threat_intel_*` tools. The tool list belongs to the upstream commit. Read it in the source of that commit when you write the manifest.

The tools `cve_search_*` call an external CVE service and the `threat_intel_*` tools call VirusTotal. A connector with a network scope that reaches only SentinelOne cannot use them. Leave them out of the manifest or add the egress.

## Not verified

- No build and no run of the image on the workstation of the author. The image workflow builds it on the first push to main. The Docker build needs network access to GitHub and to PyPI.
- That `--allow-remote-access` and the port `8080` work as the upstream entrypoint shows. The `Dockerfile` follows the script `docker-entrypoint.sh` of the same commit.
- The exact tool list of the commit. The count is from the upstream README of an earlier read.
