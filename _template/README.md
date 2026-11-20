# __NAME__

MCP server for __NAME__. It serves streamable HTTP on `:8080` at `/mcp` and answers `/healthz`.

## Environment

| Variable | Meaning |
|---|---|
| `__ENVPREFIX___BASE_URL` | Base URL of the vendor API. |
| `__ENVPREFIX___TOKEN` | Credential of the vendor API. |
| `LISTEN_ADDR` | Listen address. The default is `:8080`. |

## Tools

| Tool | Changes the target | Undo tool |
|---|---|---|
| `get_object` | No | Not needed |

## Vendor permission

State here the exact API scope or role that the tools need. Doc of the vendor: add the URL.
