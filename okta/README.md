# okta

MCP server for Okta. It reads system log events and users. It also runs response actions on users. The vendor server of Okta only reads (ADR-0180). It serves streamable HTTP on `:8080` at `/mcp` and answers `/healthz`.

## Environment

| Variable | Meaning |
|---|---|
| `OKTA_ORG_URL` | Required. Org URL, for example `https://example.okta.com`. It must use https. |
| `OKTA_API_TOKEN` | Required. An Okta API token. The server sends it as `Authorization: SSWS <token>`. |
| `OKTA_TIMEOUT` | Optional. Time limit of one vendor call. The default is `20s`. |
| `LISTEN_ADDR` | Optional. The default is `:8080`. |

## Tools

Reference: https://developer.okta.com/docs/api/openapi/okta-management/management/tag/SystemLog/, https://developer.okta.com/docs/api/openapi/okta-management/management/tag/User/, https://developer.okta.com/docs/api/openapi/okta-management/management/tag/UserSessions/ and https://developer.okta.com/docs/api/openapi/okta-management/management/tag/UserLifecycle/, and the OpenAPI spec https://github.com/okta/okta-management-openapi-spec.

| Tool | Vendor call | Undo | Annotations |
|---|---|---|---|
| `list_log_events` | `GET /api/v1/logs` | Not a change. | `readOnlyHint: true` |
| `list_users` | `GET /api/v1/users` | Not a change. | `readOnlyHint: true` |
| `get_user` | `GET /api/v1/users/{userId}` | Not a change. | `readOnlyHint: true` |
| `revoke_user_sessions` | `DELETE /api/v1/users/{userId}/sessions` | None. The user signs in again. | `destructiveHint: true`, `idempotentHint: true` |
| `suspend_user` | `POST /api/v1/users/{id}/lifecycle/suspend` | `unsuspend_user` | `destructiveHint: false`, `idempotentHint: true` |
| `unsuspend_user` | `POST /api/v1/users/{id}/lifecycle/unsuspend` | `suspend_user` | `destructiveHint: false`, `idempotentHint: true` |

The read tools return one page and never follow a page link. `list_log_events` returns at most 200 events, newest first unless `sort_order` is `ASCENDING`. When Okta has more events for the query, the result has `truncated: true` and an `after` cursor. The tool takes the cursor from the `Link` header with `rel="next"`. A caller gives it as `after` to read the next page. The log `q` input has at most 10 keywords of 40 characters each. `list_users` returns at most 50 users. Each tool returns only the fields that a defender needs.

There is no tool that reads a session. Okta has no call that lists the sessions of one user. `GET /api/v1/sessions/{sessionId}` does not work with the sessions of Okta Identity Engine, and no source shows that `authenticationContext.externalSessionId` of a log event is a valid id for it. A 404 would make the agent decide wrongly that the session does not exist. To end the sessions of a user, use `revoke_user_sessions`.

A response tool accepts a user id only: 20 letters and digits. It does not accept a login or an e-mail address. Okta refuses a second `suspend_user` on a suspended user with HTTP 400. The tool shows the Okta error, and the user stays as it was.

`revoke_user_sessions` has no undo. Okta cannot restore a session. The user restores access by signing in. The issue of this server (gibson#1548) asked for a release tool on each action. That cannot hold for a session, so the tool says so in its description and carries `destructiveHint: true`.

## Deployment requirement

The MCP endpoint on `:8080` has no caller authentication. Any caller that reaches the port can call each tool with the vendor credential of this server. The ToolHive proxy of the connector authenticates the daemon in front of it.

1. Apply a NetworkPolicy that admits ingress to this port only from the ToolHive proxy pod of the connector.
2. Do not expose the port through a Service of type `LoadBalancer` or `NodePort`, an Ingress or a port-forward.

The server has no second authentication scheme, by design. The proxy and the NetworkPolicy are the access control.

## Vendor permission

Okta lists two forms of authorization for these operations: an API token (`apiToken`) or an OAuth 2.0 token. The scopes are `okta.users.manage` for the response tools, `okta.logs.read` for `list_log_events`, `okta.users.read` for `list_users` and `get_user`.

This server uses an API token. Make the token as follows.

1. Create a service account user in Okta for this server.
2. Give it a custom admin role that holds only the permissions to suspend and unsuspend users, to clear user sessions, to read users and to read the system log, on a resource set that holds only the users that the agent may act on. Do not use the Super Administrator role.
3. Create the API token as that user. The token has the permissions of that user, and Okta blocks it when it comes from a network zone that you do not allow.

The server does not read a group. The exact permission names of the custom role are not in the public OpenAPI spec. Check them in the Okta Admin Console when you make the role.
