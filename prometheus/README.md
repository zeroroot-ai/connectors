# prometheus

MCP server for the Prometheus HTTP API. It is the health signal source of the fix loop (ADR-0179). It only reads. It serves streamable HTTP on `:8080` at `/mcp` and answers `/healthz`.

Prometheus also fits a compatible API such as Mimir, Cortex, Thanos Query and VictoriaMetrics. The tests run against a mock of the Prometheus API only.

## Environment

| Variable | Meaning |
|---|---|
| `PROMETHEUS_BASE_URL` | Required. Base URL, for example `https://prometheus.example.com`. A path prefix is allowed. |
| `PROMETHEUS_BEARER_TOKEN` | Optional. Sent as `Authorization: Bearer`. |
| `PROMETHEUS_USERNAME`, `PROMETHEUS_PASSWORD` | Optional. HTTP basic authentication. Do not set them with the bearer token. |
| `PROMETHEUS_ORG_ID` | Optional. Sent as `X-Scope-OrgID` for a multi-tenant backend. |
| `PROMETHEUS_TIMEOUT` | Optional. Time limit of one vendor call. The default is `30s`. |
| `LISTEN_ADDR` | Optional. The default is `:8080`. |

## Tools

All three tools only read. Each carries `readOnlyHint: true`. Reference: https://prometheus.io/docs/prometheus/latest/querying/api/

| Tool | Vendor call | Notes |
|---|---|---|
| `query_instant` | `POST /api/v1/query` | `query`, optional `time` and `limit`. |
| `query_range` | `POST /api/v1/query_range` | `query`, `start`, `end`, `step`. At most 31 days and 11000 points for each series. |
| `list_alerts` | `GET /api/v1/alerts` | Optional filter by `state` and `alertname`. |

A query goes in the body of a POST, because a PromQL expression can exceed the length of a URL. A POST of a query changes nothing, so the client retries it. The result holds at most `limit` series (default 100, maximum 1000).

## Vendor permission

Prometheus has no scope or role model. The server needs network access to three read endpoints: `/api/v1/query`, `/api/v1/query_range` and `/api/v1/alerts`. Put a reverse proxy in front of Prometheus and let it pass only these three paths. The server does not use the admin API, the write API or the management API (`/-/`).
