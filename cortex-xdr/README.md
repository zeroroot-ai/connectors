# cortex-xdr

MCP server for Palo Alto Networks Cortex XDR, on the public API. Class `edr`. It reads incidents, the alerts of an incident, alerts and the state of endpoints. It has no tool that changes anything. It serves streamable HTTP on `:8080` at `/mcp` and answers `/healthz`.

The official Cortex MCP server needs a build from a download of the tenant, and it has no published image or endpoint. So this server uses the documented public API.

## Environment

| Variable | Meaning |
|---|---|
| `CORTEX_API_URL` | Required. The API address of the tenant, `https://api-<fqdn>`. The fqdn belongs to the tenant. The console shows it in the cURL example of the API key. It must use https. |
| `CORTEX_API_KEY_ID` | Required. The id of the API key (a number). |
| `CORTEX_API_KEY` | Required. The API key. |
| `CORTEX_API_KEY_TYPE` | Optional. `standard` or `advanced`. The default is `standard`. |
| `CORTEX_TIMEOUT` | Optional. Time limit of one vendor call. The default is `30s`. |
| `LISTEN_ADDR` | Optional. The default is `:8080`. |

A standard key goes in the header `Authorization` as it is, with the header `x-xdr-auth-id`. An advanced key is never sent. The server sends a nonce of 64 letters and digits, the time in epoch milliseconds, and the SHA-256 hex digest of the key, the nonce and the time. The source is https://cortex-docs.paloaltonetworks.com/xdr-3-api/get-started-with-cortex-xdr-apis.md. The API allows 10 requests a second for each tenant.

## Tools

References: https://cortex-docs.paloaltonetworks.com/xdr-3-api/cortex-xdr-3.x-apis/incident-management (incidents and alerts) and https://cortex-docs.paloaltonetworks.com/xdr-3-api/cortex-xdr-3.x-apis/endpoint-management (endpoints). Each call is a `POST` with `{"request_data": {...}}` and an answer `{"reply": {...}}`.

| Tool | Vendor call | Undo | Annotations |
|---|---|---|---|
| `list_incidents` | `POST /public_api/v1/incidents/get_incidents` | Not a change. | `readOnlyHint: true` |
| `get_incident` | `POST /public_api/v1/incidents/get_incident_extra_data` | Not a change. | `readOnlyHint: true` |
| `list_alerts` | `POST /public_api/v1/alerts/get_alerts` | Not a change. | `readOnlyHint: true` |
| `list_endpoints` | `POST /public_api/v1/endpoints/get_endpoint` | Not a change. | `readOnlyHint: true` |

- Each call is a search with a `POST`. It changes nothing, and the client repeats it on a temporary error.
- The filters are typed. `status`, `severity`, `isolation` and the endpoint status take one value from the list of the API. A time is parsed as RFC 3339 and sent as epoch milliseconds. `endpoint_id` and `hostname` must match a fixed pattern. The tool writes each filter itself, so a text cannot add a filter or change an operator. `description_contains` is a text in one `contains` filter.
- A list takes `limit` (1 to 100, default 25) and `offset`. The tool sends `search_from` and `search_to`. A result has `count`, `total` and `has_more`. The API caps `total_count` at 10000.
- A list sorts by `creation_time`, newest first. The times in a result are epoch milliseconds, as the API gives them.
- `get_incident` returns the incident and up to `alerts_limit` alerts (default 25, at most 100). The API allows 10 of these calls a minute for the tenant.

## Isolate is not built

The API has `endpoints/isolate` and `endpoints/unisolate`. They need the licenses Prevent or Pro per Endpoint. This server has read tools only, as the coordinator asked for read tools first. Add the two response tools in a later change, with the undo pair that ADR-0180 asks for.

## Not verified

No call to a live tenant. The tests use an `httptest` mock that follows the pages above. The mock does not prove these points:

- The role or permission of an API key for each call. The pages say only that the key needs the RBAC permission, and they name none. Make the key with a role that can view incidents, alerts and endpoints only.
- The cap of `search_to - search_from`. The page says that the maximum result set is 100.
- The unit of `last_seen`, `creation_time` and `detection_timestamp` in an answer. The filters take epoch milliseconds. The tool returns the numbers as they come.
- The field `is_isolated` of an endpoint. The page names it, and its values are not listed.
- The format of an error body. The tool shows the HTTP status and the start of the body.

## Deployment requirement

The MCP endpoint on `:8080` has no caller authentication. Any caller that reaches the port can call each tool with the vendor credential of this server. The ToolHive proxy of the connector authenticates the daemon in front of it.

1. Apply a NetworkPolicy that admits ingress to this port only from the ToolHive proxy pod of the connector.
2. Do not expose the port through a Service of type `LoadBalancer` or `NodePort`, an Ingress or a port-forward.

The server has no second authentication scheme, by design. The proxy and the NetworkPolicy are the access control.

## Vendor permission

Make an API key for this server with a custom role that has view rights on incidents, alerts and endpoints and no response right. Prefer an advanced key, because it never sends the key itself. Do not use an administrator role.
