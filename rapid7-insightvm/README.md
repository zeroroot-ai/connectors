# rapid7-insightvm

MCP server for Rapid7 InsightVM, on the Console API v3. Class `scanner`. It reads assets, the vulnerability findings of an asset and the detail of a vulnerability. It has no tool that changes anything. It serves streamable HTTP on `:8080` at `/mcp` and answers `/healthz`.

The official Rapid7 MCP server covers Bulk Export only. It builds from source, and it has no read of the findings of an asset. So this server uses the documented REST API.

## Environment

| Variable | Meaning |
|---|---|
| `INSIGHTVM_URL` | Required. The address of the console, for example `https://insightvm.example.com:3780`. It must use https. |
| `INSIGHTVM_USER` | Required. The user name of a console account. |
| `INSIGHTVM_PASSWORD` | Required. Its password. The server sends HTTP basic authentication. |
| `INSIGHTVM_CA_FILE` | Optional. A PEM file with the certificate of the authority that signed the certificate of the console. A console often has a private certificate. The server trusts it in addition to the system roots. |
| `INSIGHTVM_TIMEOUT` | Optional. Time limit of one vendor call. The default is `30s`. |
| `LISTEN_ADDR` | Optional. The default is `:8080`. |

## Tools

References: the reference https://help.rapid7.com/insightvm/en-us/api/index.html and the OpenAPI spec https://help.rapid7.com/insightvm/en-us/api/api-v3.json.

| Tool | Vendor call | Undo | Annotations |
|---|---|---|---|
| `find_assets` | `POST /api/3/assets/search` with filters on `ip-address` (`is`) and `host-name` (`starts-with`), `match: all` | Not a change. | `readOnlyHint: true` |
| `get_asset` | `GET /api/3/assets/{id}` | Not a change. | `readOnlyHint: true` |
| `list_asset_vulnerabilities` | `GET /api/3/assets/{id}/vulnerabilities` | Not a change. | `readOnlyHint: true` |
| `get_vulnerability` | `GET /api/3/vulnerabilities/{id}` | Not a change. | `readOnlyHint: true` |

- The search is a `POST` because the API takes the filters in the body. It changes nothing, and the client repeats it on a temporary error.
- `find_assets` needs `ip_address`, `host_name` or both. It has no free filter. The tool writes the filters from typed inputs, so a text cannot add a filter or change an operator.
- Each list takes `page` (from 0) and `size` (1 to 100, default 25). A result has `page`, `total_pages` and `total_records`. The API allows 500 for `size`. The tool allows 100.
- Each asset has the count of vulnerabilities by level: `critical`, `severe`, `moderate`, `total`, `exploits` and `malware_kits`.
- `get_vulnerability` returns the CVSS v2 and v3 score and vector, the CVE ids, the dates, `fix_available` and the text of the description.
- The id of a vulnerability is a text such as `ssh-cve-2020-14145`. The spec types it as an object, so the tool turns it into text and checks that the answer has the id that was asked for.

## Not verified

No call to a live console. The tests use an `httptest` mock that follows the spec above. The mock does not prove these points:

- The type of the id of a vulnerability, which the spec leaves open.
- That `size` can be 100 on every version of the console.
- The permission names of a console role for each call.

## Deployment requirement

The MCP endpoint on `:8080` has no caller authentication. Any caller that reaches the port can call each tool with the vendor credential of this server. The ToolHive proxy of the connector authenticates the daemon in front of it.

1. Apply a NetworkPolicy that admits ingress to this port only from the ToolHive proxy pod of the connector.
2. Do not expose the port through a Service of type `LoadBalancer` or `NodePort`, an Ingress or a port-forward.

The server has no second authentication scheme, by design. The proxy and the NetworkPolicy are the access control.

## Vendor permission

Make a console user for this server with a custom role that can view assets and vulnerabilities in the sites that the agents need, and nothing else. Do not give it the Global Administrator role. A user can use two-factor authentication only with a token, which this server does not send, so use an account with no two-factor authentication and limit its network access.
