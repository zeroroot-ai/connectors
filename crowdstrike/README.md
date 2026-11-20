# crowdstrike

MCP server for response actions on CrowdStrike Falcon hosts. The vendor server `falcon-mcp` has no tool that contains a host (ADR-0180). It serves streamable HTTP on `:8080` at `/mcp` and answers `/healthz`.

## Environment

| Variable | Meaning |
|---|---|
| `FALCON_CLIENT_ID` | Required. Client id of a Falcon API client. |
| `FALCON_CLIENT_SECRET` | Required. Client secret of that API client. |
| `FALCON_BASE_URL` | Optional. The API base URL of the Falcon cloud. The default is `https://api.crowdstrike.com` (US-1). Other clouds: `https://api.us-2.crowdstrike.com`, `https://api.eu-1.crowdstrike.com`, and the gov cloud URL of CrowdStrike. |
| `FALCON_TIMEOUT` | Optional. Time limit of one vendor call. The default is `20s`. |
| `LISTEN_ADDR` | Optional. The default is `:8080`. |

The variable names match `falcon-mcp`, so one credential secret can serve both servers.

## Tools

| Tool | Vendor call | Undo | Annotations |
|---|---|---|---|
| `contain_host` | `POST /devices/entities/devices-actions/v2?action_name=contain` | `lift_containment` | `destructiveHint: false`, `idempotentHint: true` |
| `lift_containment` | `POST /devices/entities/devices-actions/v2?action_name=lift_containment` | `contain_host` | `destructiveHint: false`, `idempotentHint: true` |

A tool accepts the agent id (AID) of a host only: 32 hexadecimal characters. It does not accept a host name or a list. One call acts on one host.

Falcon answers HTTP 202. The request is queued, and the change takes effect when the sensor of the host gets it. The tool reports `accepted: true` only when Falcon lists the host in `resources` and returns no error. The tool does not wait for the sensor and does not read the state of the host.

The CrowdStrike API reference needs a Falcon login. The request and response shapes come from the open source clients of CrowdStrike: `falconpy` (endpoint `PerformActionV2`) and `gofalcon` (models `MsaEntityActionRequestV2` and `MsaReplyAffectedEntities`). The mock of the tests follows those shapes. No test ran against a Falcon tenant.

## Deployment requirement

The MCP endpoint on `:8080` has no caller authentication. Any caller that reaches the port can call each tool with the vendor credential of this server. The ToolHive proxy of the connector authenticates the daemon in front of it.

1. Apply a NetworkPolicy that admits ingress to this port only from the ToolHive proxy pod of the connector.
2. Do not expose the port through a Service of type `LoadBalancer` or `NodePort`, an Ingress or a port-forward.

The server has no second authentication scheme, by design. The proxy and the NetworkPolicy are the access control.

## Vendor permission

Create a Falcon API client with the scope `Hosts: Write` and no other scope. Falcon documents this scope for host actions (contain and lift containment). Restrict the API client to the host groups that the agent may act on when your plan supports it.
