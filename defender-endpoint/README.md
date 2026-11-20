# defender-endpoint

MCP server for response actions on Microsoft Defender for Endpoint devices. The vendor tools in the triage collection of the Microsoft Sentinel MCP server only read (ADR-0180). It serves streamable HTTP on `:8080` at `/mcp` and answers `/healthz`.

## Environment

| Variable | Meaning |
|---|---|
| `DEFENDER_TENANT_ID` | Required. Tenant id (GUID) or domain. |
| `DEFENDER_CLIENT_ID` | Required. Application (client) id of the app registration. |
| `DEFENDER_CLIENT_SECRET` | Required. Client secret of the app registration. |
| `DEFENDER_API_URL` | Optional. The default is `https://api.security.microsoft.com`. Use a regional host (`us.api.security.microsoft.com`, `eu.api.security.microsoft.com`, and others) or the US Government URL when needed. |
| `DEFENDER_AUTHORITY_URL` | Optional. The default is `https://login.microsoftonline.com`. |
| `DEFENDER_SCOPE` | Optional. The default is `https://api.securitycenter.microsoft.com/.default`, the resource that Microsoft names for the token. |
| `DEFENDER_TIMEOUT` | Optional. Time limit of one vendor call. The default is `20s`. |
| `LISTEN_ADDR` | Optional. The default is `:8080`. |

## Tools

References: https://learn.microsoft.com/en-us/defender-endpoint/api/isolate-machine and https://learn.microsoft.com/en-us/defender-endpoint/api/unisolate-machine

| Tool | Vendor call | Undo | Annotations |
|---|---|---|---|
| `isolate_device` | `POST /api/machines/{id}/isolate` | `release_device` | `destructiveHint: false`, `idempotentHint: true` |
| `release_device` | `POST /api/machines/{id}/unisolate` | `isolate_device` | `destructiveHint: false`, `idempotentHint: true` |

A tool accepts the device id of Defender only: 40 hexadecimal characters. It does not accept a host name. The tool stores the comment of the call, with the prefix `Zero Root: `, on the machine action. Defender answers HTTP 201 with a machine action that has the status `Pending`. The device runs the action when it is online. The tool does not wait and does not poll the status.

Microsoft limits both calls to 100 for each minute and 1500 for each hour. Defender refuses a second release for a device while a release is pending.

## Vendor permission

Grant the application permission `Machine.Isolate` of the API `WindowsDefenderATP` ("Isolate machine") to the app, with admin consent, and no other permission. This is the only permission that the two pages above name for both calls.

When you restrict the agent to some devices, use device groups and role-based access control of Defender. A token with application permissions acts on all devices.
