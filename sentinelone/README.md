# sentinelone

MCP server for SentinelOne. It reads threats and agent state. It also runs response actions on agents. It serves streamable HTTP on `:8080` at `/mcp` and answers `/healthz`.

## Environment

| Variable | Meaning |
|---|---|
| `SENTINELONE_CONSOLE_URL` | Required. URL of the management console, for example `https://usea1-example.sentinelone.net`. It must use https. |
| `SENTINELONE_API_TOKEN` | Required. API token of a service user. The server sends it as `Authorization: ApiToken <token>`. |
| `SENTINELONE_TIMEOUT` | Optional. Time limit of one vendor call. The default is `20s`. |
| `LISTEN_ADDR` | Optional. The default is `:8080`. |

## Tools

| Tool | Vendor call | Undo | Annotations |
|---|---|---|---|
| `list_threats` | `GET /web/api/v2.1/threats` | Not a change. | `readOnlyHint: true` |
| `list_agents` | `GET /web/api/v2.1/agents` | Not a change. | `readOnlyHint: true` |
| `disconnect_from_network` | `POST /web/api/v2.1/agents/actions/disconnect` | `reconnect_to_network` | `destructiveHint: false`, `idempotentHint: true` |
| `reconnect_to_network` | `POST /web/api/v2.1/agents/actions/connect` | `disconnect_from_network` | `destructiveHint: false`, `idempotentHint: true` |

The read tools return one page of at most 100 items. A result holds `next_cursor`, `total_items` and `truncated`. `truncated` is true when the result holds only a part of the list: SentinelOne gave a next cursor, or the first page has fewer items than the total. An agent must not take such a result for the whole list. A caller gives `next_cursor` as `cursor` to read the next page. Each tool returns only the fields that a defender needs.

`list_threats` filters by threat id, agent id, incident status, mitigation status and creation time. It sends the query parameters `ids`, `agentIds`, `incidentStatuses`, `mitigationStatuses`, `createdAt__gte`, `createdAt__lte`, `limit` and `cursor`. `list_agents` filters by agent id, exact computer name, network status and activity. It sends `ids`, `computerName`, `networkStatuses`, `isActive`, `limit` and `cursor`.

The response fields come from the public pages https://docs.blinkops.com/docs/integrations/sentinelone/actions/get-threats and https://xsoar.pan.dev/docs/reference/integrations/sentinel-one-v2. The names of the query parameters are not in a public page that this repo can cite. They are not checked against the official reference. No test ran against a SentinelOne console. When SentinelOne refuses a filter, the tool shows its error.

A response tool accepts one agent id: a decimal number. It does not accept a host name, a list or a filter. The request body is `{"filter": {"ids": ["<agent id>"]}, "data": {}}`. The answer is `{"data": {"affected": n}}`. When `affected` is 0, SentinelOne changed no agent: the id is unknown, or the agent is already in that state. The tool then reports `affected: 0` and a note, and it is not an error.

The reference of SentinelOne needs a console login. The request and response shapes come from public clients of the API: the PowerShell module `PS-SentinelOne` and the SentinelOne extension of LimaCharlie. The mock of the tests follows those shapes. No test ran against a SentinelOne console. The array form of `filter.ids` is not checked against the official reference.

## Deployment requirement

The MCP endpoint on `:8080` has no caller authentication. Any caller that reaches the port can call each tool with the vendor credential of this server. The ToolHive proxy of the connector authenticates the daemon in front of it.

1. Apply a NetworkPolicy that admits ingress to this port only from the ToolHive proxy pod of the connector.
2. Do not expose the port through a Service of type `LoadBalancer` or `NodePort`, an Ingress or a port-forward.

The server has no second authentication scheme, by design. The proxy and the NetworkPolicy are the access control.

## Vendor permission

Create a service user for this server in SentinelOne. Give it a custom role that holds only the permissions to view threats, to view endpoints, to disconnect an endpoint from the network and to connect it back, and scope it to the sites that the agent may act on. Create the API token for that service user. Do not use an Admin role.

The exact names of the two role permissions are in the console, not in a public page that this repo can cite. Check them when you make the role.
