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

## Deployment requirement

The MCP endpoint on `:8080` has no caller authentication. Any caller that reaches the port can call each tool with the vendor credential of this server. The ToolHive proxy of the connector authenticates the daemon in front of it.

1. Apply a NetworkPolicy that admits ingress to this port only from the ToolHive proxy pod of the connector.
2. Do not expose the port through a Service of type `LoadBalancer` or `NodePort`, an Ingress or a port-forward.

The server has no second authentication scheme, by design. The proxy and the NetworkPolicy are the access control.

## Vendor permission

State here the exact API scope or role that the tools need. Doc of the vendor: add the URL.
