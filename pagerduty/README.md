# pagerduty

MCP server for paging with PagerDuty. Class `paging`. It sends alerts through the Events API v2 and reads incidents through the REST API v2. The hosted PagerDuty MCP server does not publish its tool arguments, so this server has fixed tool names. It serves streamable HTTP on `:8080` at `/mcp` and answers `/healthz`.

## Environment

| Variable | Meaning |
|---|---|
| `PAGERDUTY_ROUTING_KEY` | Required. The integration key (routing key) of an Events API v2 integration of one service. It has 32 characters. One server serves one service. |
| `PAGERDUTY_API_TOKEN` | Required. A REST API key. Make it a read-only General Access key. The server sends `Authorization: Token token=<key>`. |
| `PAGERDUTY_EVENTS_URL` | Optional. The default is `https://events.pagerduty.com`. For the EU service region use `https://events.eu.pagerduty.com`. |
| `PAGERDUTY_API_URL` | Optional. The default is `https://api.pagerduty.com`. For the EU service region use `https://api.eu.pagerduty.com`. |
| `PAGERDUTY_SERVICE_ID` | Optional. The id of the service that the routing key belongs to. When set, `find_incidents` lists only the incidents of this service, with `service_ids[]`. Set it, because the same `dedup_key` on another service is another alert. |
| `PAGERDUTY_TIMEOUT` | Optional. Time limit of one vendor call. The default is `20s`. |
| `LISTEN_ADDR` | Optional. The default is `:8080`. |

Both URLs must use https. The routing key and the API key never appear in a tool input, a result or a log.

## Tools

References: https://docs.pagerduty.com/developer/api/reference/events-v2/send-event/create-v2-event, https://docs.pagerduty.com/developer/send-alert-event.md, https://docs.pagerduty.com/developer/api/reference/rest/incidents/get-incident and https://docs.pagerduty.com/developer/api/reference/rest/incidents/list-incidents.

| Tool | Vendor call | Undo | Annotations |
|---|---|---|---|
| `trigger_alert` | `POST /v2/enqueue` with `event_action: trigger` | `resolve_alert`. The page that PagerDuty sent stays sent. | `destructiveHint: false`, `idempotentHint: true` |
| `acknowledge_alert` | `POST /v2/enqueue` with `event_action: acknowledge` | None. | `destructiveHint: true`, `idempotentHint: true` |
| `resolve_alert` | `POST /v2/enqueue` with `event_action: resolve` | None. A later trigger with the same `dedup_key` opens a new alert. | `destructiveHint: true`, `idempotentHint: true` |
| `get_incident` | `GET /incidents/{id}` | Not a change. | `readOnlyHint: true` |
| `find_incidents` | `GET /incidents?incident_key=...&date_range=all&limit=10` | Not a change. | `readOnlyHint: true` |

- `dedup_key` is required for each alert tool, also for a trigger. A trigger with a `dedup_key` can be repeated with the same effect, so the client repeats it on a temporary error. A trigger without a key would make a new alert on each repeat.
- The result of an alert tool holds `dedup_key`, `action` and the message of PagerDuty. The Events API gives no incident id. Call `find_incidents` with the `dedup_key` to get the incident. PagerDuty can need a moment to make it.
- `summary` has at most 1024 characters, `dedup_key` at most 255. `severity` is `critical`, `error`, `warning` or `info`. The default is `error`. `custom_details` has at most 20 pairs of text.
- PagerDuty drops an acknowledge or a resolve for a `dedup_key` that has no open alert, and the Events API still answers 202. The tool then reports success. Read the incident to see the state.
- The Events API allows about 120 events a minute for each integration key, and a 429 means that the event was not taken. The client retries a 429 after the wait that PagerDuty asks for.

## Grouping of alerts

PagerDuty groups alerts into incidents. The list-incidents page says that an incident with child alerts has no incident key, so a query by `incident_key` returns the incidents whose alerts match the key. `find_incidents` therefore returns the incidents that hold the alert, and an incident can hold other alerts too. Check `service_id` and `dedup_key` in the result. The parameter `service_ids[]` is on the page https://docs.pagerduty.com/developer/api/reference/rest/incidents/list-incidents. It is sent only when `PAGERDUTY_SERVICE_ID` is set.

## Not verified

No call to a live PagerDuty. The tests use an `httptest` mock that follows the PagerDuty pages above. The mock does not prove these points:

- The body of a 400 answer. The pages that were read do not show it. The tool shows the HTTP status and the start of the body.
- That the path `/v2/enqueue` works on the EU host. The pages that were read give the EU host and no path.
- That `GET /incidents/{id}` takes an incident number as well as an id. The page says it does. The tool accepts capital letters and digits only.
- The format of an incident id beyond the example `PT4KHLK`.

## Deployment requirement

The MCP endpoint on `:8080` has no caller authentication. Any caller that reaches the port can call each tool with the vendor credential of this server. The ToolHive proxy of the connector authenticates the daemon in front of it.

1. Apply a NetworkPolicy that admits ingress to this port only from the ToolHive proxy pod of the connector.
2. Do not expose the port through a Service of type `LoadBalancer` or `NodePort`, an Ingress or a port-forward.

The server has no second authentication scheme, by design. The proxy and the NetworkPolicy are the access control.

## Vendor permission

Make an Events API v2 integration on the service that the agents page. Its key is the routing key. Make a General Access REST API key with the box "Read-only API Key" checked. A read-only key limits the REST calls to GET. Do not give the server a key that can change incidents.
