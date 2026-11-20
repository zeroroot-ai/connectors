# entra-id

MCP server for response actions on Microsoft Entra ID users, through Microsoft Graph. The vendor server of Microsoft (the MCP Server for Enterprise) only reads (ADR-0180). It serves streamable HTTP on `:8080` at `/mcp` and answers `/healthz`.

## Environment

| Variable | Meaning |
|---|---|
| `ENTRA_TENANT_ID` | Required. Tenant id (GUID) or domain. |
| `ENTRA_CLIENT_ID` | Required. Application (client) id of the app registration. |
| `ENTRA_CLIENT_SECRET` | Required. Client secret of the app registration. |
| `ENTRA_GRAPH_URL` | Optional. The default is `https://graph.microsoft.com`. Set it for a national cloud. |
| `ENTRA_AUTHORITY_URL` | Optional. The default is `https://login.microsoftonline.com`. Set it for a national cloud. |
| `ENTRA_TIMEOUT` | Optional. Time limit of one vendor call. The default is `20s`. |
| `LISTEN_ADDR` | Optional. The default is `:8080`. |

The server gets a token with the client credentials grant and the scope `https://graph.microsoft.com/.default`. It renews the token before it expires.

## Tools

Reference: https://learn.microsoft.com/en-us/graph/api/user-revokesigninsessions and https://learn.microsoft.com/en-us/graph/api/user-update

| Tool | Vendor call | Undo | Annotations |
|---|---|---|---|
| `revoke_sign_in_sessions` | `POST /v1.0/users/{id}/revokeSignInSessions` | None. The user signs in again. | `destructiveHint: true`, `idempotentHint: true` |
| `disable_account` | `PATCH /v1.0/users/{id}` with `{"accountEnabled": false}` | `enable_account` | `destructiveHint: false`, `idempotentHint: true` |
| `enable_account` | `PATCH /v1.0/users/{id}` with `{"accountEnabled": true}` | `disable_account` | `destructiveHint: false`, `idempotentHint: true` |

A tool accepts the object id of a user (a GUID) only. It does not accept a user principal name.

Two points of Microsoft behavior matter for a response. A disabled account does not end a session that exists, so call `revoke_sign_in_sessions` after `disable_account`. A revoke does not invalidate an access token before it expires, and tokens can take a few minutes to stop. The gibson#1549 issue asked for a release tool on each action. A session has none, so `revoke_sign_in_sessions` says so and carries `destructiveHint: true`.

## Vendor permission

Grant these Microsoft Graph application permissions to the app, with admin consent, and no other.

| Tool | Application permission (least privileged) |
|---|---|
| `revoke_sign_in_sessions` | `User.RevokeSessions.All` |
| `disable_account`, `enable_account` | `User.EnableDisableAccount.All` and `User.Read.All` |

For `accountEnabled`, Microsoft also requires that an app-only caller holds a higher administrator role (see "Who can perform sensitive actions" at https://learn.microsoft.com/en-us/graph/api/resources/users). Assign the role of the least rank that covers the users that the agent may disable. Use User Administrator for non-administrator users. Use Privileged Authentication Administrator to cover administrators. Scope the role to an administrative unit when the agent may act on a part of the tenant only.
