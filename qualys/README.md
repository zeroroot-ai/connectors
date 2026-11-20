# qualys

MCP server for Qualys VMDR. It reads the latest vulnerability detections of a host. Qualys publishes no MCP server for a customer (ADR-0177). It only reads. It serves streamable HTTP on `:8080` at `/mcp` and answers `/healthz`.

## Environment

| Variable | Meaning |
|---|---|
| `QUALYS_API_URL` | Required. The API server URL of your Qualys platform, for example `https://qualysapi.qualys.com`. It must use https. |
| `QUALYS_USERNAME` | Required. User name of a Qualys user with API access. |
| `QUALYS_PASSWORD` | Required. Password of that user. |
| `QUALYS_TIMEOUT` | Optional. Time limit of one vendor call. The default is `60s`. |
| `LISTEN_ADDR` | Optional. The default is `:8080`. |

The server sends HTTP basic authentication and the header `X-Requested-With`, which Qualys requires.

## Tools

Reference: "Host List Detection" in the Qualys VM/PC API User Guide, https://cdn2.qualys.com/docs/qualys-api-vmpc-user-guide.pdf

| Tool | Vendor call | Annotations |
|---|---|---|
| `get_host_detections` | `GET /api/2.0/fo/asset/host/vm/detection/?action=list` | `readOnlyHint: true` |

The tool takes one host id or one IP address, and optional filters: `status`, `min_severity`, `include_results` and `limit`. It never sends a range, a list or a CIDR block. The answer of Qualys is XML. The server turns it into JSON. By default it leaves out the scan output of each detection (`show_results=0`).

A detection is a flaw on a host, so a reader can turn it into a finding. A fixed detection stays in the list when you ask for the status `Fixed`. The tool does not use the Knowledge Base API, so it does not return the title or the CVE of a QID.

## Vendor permission

Use a Qualys user with the role Reader and the permission "API Access", and assign it only the asset groups that the agent may read. The guide says that the API returns only the hosts that are assigned to the user through asset groups. A Reader needs no write permission. Do not use a Manager.

Qualys limits the number of API calls for each subscription and the number of concurrent calls for each user. The server does not retry a call that Qualys refuses for that reason (HTTP 409). It shows the Qualys error instead.
