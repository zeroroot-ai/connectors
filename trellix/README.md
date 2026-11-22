# trellix

MCP server for Trellix EDR. Class `edr`. It reads detections (alerts) and the state of hosts. It has no isolate tool and no release tool. It serves streamable HTTP on `:8080` at `/mcp` and answers `/healthz`.

## Isolate is not built

The isolate call is documented: `POST /edr/v2/remediation/host` with the action `quarantineHost` and `hostIds` (https://docs.trellix.com/docs/post-host-remediation-1.md). The release call is not. Only a release note names an action `UnquarantineHost`, and no page shows its request body or the exact spelling. Next to the example `quarantineHost`, the spelling of the release action is not safe to guess. An isolate tool with no confirmed release breaks ADR-0180, which asks for a named undo. So the server has no isolate tool until a tenant confirms the release call (gibson#1561).

## Environment

| Variable | Meaning |
|---|---|
| `TRELLIX_CLIENT_ID` | Required. The client id from the developer portal of Trellix. |
| `TRELLIX_CLIENT_SECRET` | Required. The client secret. The server sends it in the authorization header of the token request ("Auth Style: In Header"). |
| `TRELLIX_API_KEY` | Required. The API key. The server sends it as `x-api-key`. It comes from the onboarding e-mail or the API Access Management page. |
| `TRELLIX_API_URL` | Optional. The gateway. The default is `https://api.manage.trellix.com`. The onboarding e-mail gives the gateway of your tenant. |
| `TRELLIX_IAM_URL` | Optional. The default is `https://iam.mcafee-cloud.com/iam/v1.1/token`. |
| `TRELLIX_SCOPES` | Optional. The scopes of the token. The default is the list of the Trellix page: `epo.evt.r,epo.device.r,mi.user.investigate,soc.rts.r,soc.hts.r`. It is the list of the Trellix page without `soc.rts.c`, which creates. |
| `TRELLIX_TIMEOUT` | Optional. Time limit of one vendor call. The default is `20s`. |
| `LISTEN_ADDR` | Optional. The default is `:8080`. |

Both URLs must use https. The server gets a token with the client credentials grant and keeps it until shortly before it expires.

## Tools

References: https://docs.trellix.com/docs/get-alerts-1.md, https://docs.trellix.com/docs/get-host-info-1.md and https://docs.trellix.com/network-and-platform/docs/trellix-edr.md.

| Tool | Vendor call | Undo | Annotations |
|---|---|---|---|
| `list_alerts` | `GET /edr/v3/alerts` with `from`, `to`, `page[offset]`, `page[limit]` | Not a change. | `readOnlyHint: true` |
| `list_hosts` | `GET /edr/v2/remediation/host-info` with `aGuid`, `hostName`, `page[offset]`, `page[limit]` | Not a change. | `readOnlyHint: true` |

- `list_alerts` takes `since` and `until` (RFC 3339, sent as epoch milliseconds), `limit` (1 to 100, default 25) and `offset`. Each alert has the rule, the severity (`s0` to `s5`), the score, the process, the command line, the user, the MITRE tags and the host with its agent GUID (`MAGUID`). The field `HostInfo` of the vendor, a serialized text, is not returned.
- `list_hosts` takes `agent_guid` for one host or `host_name` for a prefix search. Each host has the GUID, the name, the operating system, the last boot time and the network interfaces.
- The page of `GET /edr/v3/alerts` does not state the syntax of `filter` and `sort`. The tool does not send them. A caller filters the result.
- `has_more` is true when more records exist. The links of an answer show a next link on the last page too, so the tool does not use them. The alerts answer of the example has no total. Then a full page means that more can exist.
- `GET /edr/v3/alerts` is functionally the same as `GET /edr/v2/alerts` and adds the host information. The tool uses v3.

## Not verified

No call to a live Trellix tenant. The tests use an `httptest` mock that follows the pages above. The mock does not prove these points:

- Which scopes the two reads need. The Trellix page says to select all EDR scopes. The default list is the Trellix list without `soc.rts.c`, which can create. Cut the list to the least that works on your tenant, and set `TRELLIX_SCOPES`.
- That a comma between scopes is accepted by the token endpoint. The page of Trellix writes the list with commas, and the OAuth standard uses a space.
- The maximum value of `page[limit]`. The tool allows 100.
- The format of an error body. The tool shows the HTTP status and the start of the body.

## Deployment requirement

The MCP endpoint on `:8080` has no caller authentication. Any caller that reaches the port can call each tool with the vendor credential of this server. The ToolHive proxy of the connector authenticates the daemon in front of it.

1. Apply a NetworkPolicy that admits ingress to this port only from the ToolHive proxy pod of the connector.
2. Do not expose the port through a Service of type `LoadBalancer` or `NodePort`, an Ingress or a port-forward.

The server has no second authentication scheme, by design. The proxy and the NetworkPolicy are the access control.

## Vendor permission

Make the client credentials in the developer portal for this server only. Give them the read scopes of the EDR alerts and the host inventory, and no scope that creates a job or a remediation. Keep the API key in the secret store of the connector.
