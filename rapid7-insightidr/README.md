# rapid7-insightidr

MCP server for Rapid7 InsightIDR, on the Insight Platform API. Class `siem`. It reads investigations and the alerts of an investigation. It has no tool that changes anything. It serves streamable HTTP on `:8080` at `/mcp` and answers `/healthz`.

The official Rapid7 MCP server covers Bulk Export only. So this server uses the documented REST API.

## Environment

| Variable | Meaning |
|---|---|
| `INSIGHTIDR_API_KEY` | Required. An Insight Platform API key. The server sends it as `X-Api-Key`. |
| `INSIGHTIDR_REGION` | Optional. The region code of your platform: `us`, `us2`, `us3`, `eu`, `ca`, `au`, `ap`, `me1` or `aps2`. The default is `us`. The host is `https://<region>.api.insight.rapid7.com`. |
| `INSIGHTIDR_URL` | Optional. A full base URL that replaces the host of the region. It must use https. |
| `INSIGHTIDR_TIMEOUT` | Optional. Time limit of one vendor call. The default is `30s`. |
| `LISTEN_ADDR` | Optional. The default is `:8080`. |

## Tools

References: the OpenAPI spec https://help.rapid7.com/insightidr/en-us/api/v2/insightidr-api-v2.json, the reference https://help.rapid7.com/insightidr/en-us/api/v2/docs.html, https://docs.rapid7.com/insight/product-apis/ and https://docs.rapid7.com/insight/api-overview/.

| Tool | Vendor call | Undo | Annotations |
|---|---|---|---|
| `list_investigations` | `GET /idr/v2/investigations` with `statuses`, `priorities`, `sources`, `start_time`, `end_time`, `index`, `size` | Not a change. | `readOnlyHint: true` |
| `get_investigation` | `GET /idr/v2/investigations/{id}` | Not a change. | `readOnlyHint: true` |
| `list_investigation_alerts` | `GET /idr/v2/investigations/{id}/alerts` with `index`, `size` | Not a change. | `readOnlyHint: true` |

- The id of an investigation is its `rrn`, for example `rrn:investigation:us1:<organization>:investigation:6A74T2A4`. The v2 schema has no `id` field. `get_investigation` and `list_investigation_alerts` take the rrn or an id.
- `status` is `OPEN`, `INVESTIGATING` or `CLOSED`. `priority` is `CRITICAL`, `HIGH`, `MEDIUM`, `LOW` or `UNSPECIFIED`. `source` is `MANUAL`, `HUNT` or `ALERT`. The tool refuses another value. Each input takes one value.
- Each list takes `index` (the page, from 0) and `size` (1 to 100, default 20). A result has `index`, `total_pages` and `total_records`.
- InsightIDR uses 28 days before now as the default `start_time`.
- The spec types `detectionRuleRrn` of an alert as an object `{ruleName, ruleRrn}`, and `ruleRrn` as an rrn object. The tool returns `detection_rule_name` and `detection_rule_rrn`. It takes `ruleRrn` as text when it is text, and joins its parts when it is an object. The rrn of an investigation is read the same way.
- The spec types the assignee as text or null. The tool reads a text, or the field `email` or `name` of an object.

## Alerts of the alert triage API are not built

InsightIDR has a second alert API, the alert triage API, for customers of Managed Detection and Response. Its operation names are known (`searchAlerts`, `getAlert`, and others). No page that was read shows its paths, methods or fields, so no tool for it is written. The alerts here are the alerts of an investigation. Add the other API when its reference is confirmed.

## Not verified

No call to a live Insight Platform. The tests use an `httptest` mock that follows the spec above. The mock does not prove these points:

- Which API key type each call needs. A user key has the permissions of its owner. An organization key works across the products.
- The shape of the assignee when it is an object.
- Whether the `rrn` of the list is text on every region. The spec types the `rrn` of one investigation as an object. The tool uses the text of the list and the id that the caller gave for one investigation.

## Deployment requirement

The MCP endpoint on `:8080` has no caller authentication. Any caller that reaches the port can call each tool with the vendor credential of this server. The ToolHive proxy of the connector authenticates the daemon in front of it.

1. Apply a NetworkPolicy that admits ingress to this port only from the ToolHive proxy pod of the connector.
2. Do not expose the port through a Service of type `LoadBalancer` or `NodePort`, an Ingress or a port-forward.

The server has no second authentication scheme, by design. The proxy and the NetworkPolicy are the access control.

## Vendor permission

Make a user key of a service account that can only view investigations in InsightIDR. A user key has the permissions of the user, so give the user no administrator role. Do not use an organization key.
