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
| `PROMETHEUS_HEALTH_QUERIES` | Optional. A JSON object that maps the key of an application to the PromQL query that the tenant states as its health. Used by `health_signal`. |
| `PROMETHEUS_TIMEOUT` | Optional. Time limit of one vendor call. The default is `30s`. |
| `LISTEN_ADDR` | Optional. The default is `:8080`. |

## Tools

All four tools only read. Each carries `readOnlyHint: true`. Reference: https://prometheus.io/docs/prometheus/latest/querying/api/

| Tool | Vendor call | Notes |
|---|---|---|
| `query_instant` | `POST /api/v1/query` | `query`, optional `time` and `limit`. |
| `query_range` | `POST /api/v1/query_range` | `query`, `start`, `end`, `step`. At most 31 days and 11000 points for each series. |
| `list_alerts` | `GET /api/v1/alerts` | Optional filter by `state` and `alertname`. |
| `health_signal` | `POST /api/v1/query_range` | `application`, `since`, `hold_seconds`. The Gibson daemon calls it. |

A query goes in the body of a POST, because a PromQL expression can exceed the length of a URL. A POST of a query changes nothing, so the client retries it. The result holds at most `limit` series (default 100, maximum 1000).

## Health signal

The Gibson daemon calls `health_signal` after a deploy to decide if a fix held (`ToolHealthSignal` in `internal/engine/fixloop/connector.go` of zeroroot-ai/gibson). It sends `application`, `since` and `hold_seconds`. The tool returns `known`, `done`, `held` and `at`. It also returns `value` and `reason`.

The tenant states the health of each application. Set `PROMETHEUS_HEALTH_QUERIES` to a JSON object. The key is the application key that `deployment_state` returns (`group/project` on GitLab, `owner/repo` on GitHub). The value is one PromQL query that is healthy while its value is not 0. Use the `bool` modifier on a comparison:

```json
{"acme/shop": "sum(rate(http_requests_total{code=~\"5..\"}[1m])) / sum(rate(http_requests_total[1m])) < bool 0.01"}
```

The tool runs the query as a range query from `since` to `since + hold_seconds`, or to now when the hold time still runs. The step is 15 seconds or more.

| Case | `known` | `done` | `held` |
|---|---|---|---|
| The tenant stated no query for the application. | false | false | false |
| The hold time has not started, or it still runs and no sample was 0. | true | false | false |
| One sample was 0 or NaN. The first one ends the hold time at once. `at` and `value` show it. | true | true | false |
| The hold time ended and the query returned no data. | true | true | false |
| The hold time ended and a series has samples for less than 90 percent of it. | true | true | false |
| The hold time ended and every sample was not 0. | true | true | true |

An absent metric is a failure and not a pass. A service that stops to report fails its hold time.

## Vendor permission

Prometheus has no scope or role model. The server needs network access to three read endpoints: `/api/v1/query`, `/api/v1/query_range` and `/api/v1/alerts`. Put a reverse proxy in front of Prometheus and let it pass only these three paths. The server does not use the admin API, the write API or the management API (`/-/`).
