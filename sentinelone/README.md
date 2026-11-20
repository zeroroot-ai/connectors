# sentinelone

MCP server for response actions on SentinelOne agents. The vendor server `purple-mcp` is read only (ADR-0180). It serves streamable HTTP on `:8080` at `/mcp` and answers `/healthz`.

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
| `disconnect_from_network` | `POST /web/api/v2.1/agents/actions/disconnect` | `reconnect_to_network` | `destructiveHint: false`, `idempotentHint: true` |
| `reconnect_to_network` | `POST /web/api/v2.1/agents/actions/connect` | `disconnect_from_network` | `destructiveHint: false`, `idempotentHint: true` |

A tool accepts one agent id: a decimal number. It does not accept a host name, a list or a filter. The request body is `{"filter": {"ids": ["<agent id>"]}, "data": {}}`. The answer is `{"data": {"affected": n}}`. When `affected` is 0, SentinelOne changed no agent: the id is unknown, or the agent is already in that state. The tool then reports `affected: 0` and a note, and it is not an error.

The reference of SentinelOne needs a console login. The request and response shapes come from public clients of the API: the PowerShell module `PS-SentinelOne` and the SentinelOne extension of LimaCharlie. The mock of the tests follows those shapes. No test ran against a SentinelOne console. The array form of `filter.ids` is not checked against the official reference.

## Deployment requirement

The MCP endpoint on `:8080` has no caller authentication. Any caller that reaches the port can call each tool with the vendor credential of this server. The ToolHive proxy of the connector authenticates the daemon in front of it.

1. Apply a NetworkPolicy that admits ingress to this port only from the ToolHive proxy pod of the connector.
2. Do not expose the port through a Service of type `LoadBalancer` or `NodePort`, an Ingress or a port-forward.

The server has no second authentication scheme, by design. The proxy and the NetworkPolicy are the access control.

## Vendor permission

Create a service user for this server in SentinelOne. Give it a custom role that holds only the permissions to disconnect an endpoint from the network and to connect it back, and scope it to the sites that the agent may act on. Create the API token for that service user. Do not use an Admin role.

The exact names of the two role permissions are in the console, not in a public page that this repo can cite. Check them when you make the role.
