# jira

MCP server for issues in Jira Cloud, on the REST API v3. Class `ticket`. The tool names, the arguments and the results follow the ticket contract of the repo (see `README.md` of the repo, section "Ticket contract"). It serves streamable HTTP on `:8080` at `/mcp` and answers `/healthz`.

The hosted Atlassian MCP server does not publish its tool arguments, and a wrong argument name can fail without a notice. This server uses the documented REST API with fixed tool names.

## Environment

| Variable | Meaning |
|---|---|
| `JIRA_SITE_URL` | Required. The address of the site, for example `https://example.atlassian.net`. It must use https. |
| `JIRA_EMAIL` | Required. The e-mail address of the account that owns the API token. |
| `JIRA_API_TOKEN` | Required. An Atlassian API token. The server sends HTTP basic authentication with the e-mail address and the token. |
| `JIRA_ISSUE_TYPE` | Optional. The issue type of `create_ticket`, by name. The default is `Task`. |
| `JIRA_RESOLUTION` | Optional. The resolution that `close_ticket` sets, by name, for example `Done`. The default sets none. |
| `JIRA_TIMEOUT` | Optional. Time limit of one vendor call. The default is `20s`. |
| `LISTEN_ADDR` | Optional. The default is `:8080`. |

## Tools

Reference: https://developer.atlassian.com/cloud/jira/platform/rest/v3/ and the OpenAPI spec https://dac-static.atlassian.com/cloud/jira/platform/swagger-v3.v3.json. Authentication: https://developer.atlassian.com/cloud/jira/platform/basic-auth-for-rest-apis/.

| Tool | Vendor call | Undo | Annotations |
|---|---|---|---|
| `create_ticket` | `POST /rest/api/3/issue` | `close_ticket` | `destructiveHint: false`, `idempotentHint: false` |
| `update_ticket` | `PUT /rest/api/3/issue/{key}` | `update_ticket` with the old value | `destructiveHint: false`, `idempotentHint: true` |
| `add_comment` | `POST /rest/api/3/issue/{key}/comment` | None. Only a person deletes a comment. | `destructiveHint: true`, `idempotentHint: false` |
| `close_ticket` | `GET` then `POST /rest/api/3/issue/{key}/transitions`, and a comment | A person reopens it, or a transition back | `destructiveHint: false`, `idempotentHint: true` |
| `get_ticket` | `GET /rest/api/3/issue/{key}` | Not a change. | `readOnlyHint: true` |
| `search_tickets` | `POST /rest/api/3/search/jql` | Not a change. | `readOnlyHint: true` |
| `list_transitions` | `GET /rest/api/3/issue/{key}/transitions` | Not a change. | `readOnlyHint: true` |
| `transition_ticket` | `POST /rest/api/3/issue/{key}/transitions` | A transition back, when the workflow has one | `destructiveHint: false`, `idempotentHint: false` |

The last two tools are not in the contract, because it has no tool for a named transition.

How the contract maps to Jira:

- `ticket_id` and `number` are the issue key, for example `SEC-12`. A numeric id, a path or a lower-case key is refused.
- `queue` is the project key. `create_ticket` needs it. The issue type comes from `JIRA_ISSUE_TYPE`.
- `priority`: `low` is Low, `medium` is Medium, `high` is High and `critical` is Highest. A project with other priority names makes Jira refuse the call, and the tool shows the error.
- `description`, `body` and the resolution are plain text. The server writes them as a document of the Atlassian Document Format with one paragraph for each line. `get_ticket` reads the text of a document again.
- `closed` is true when the category of the status is `done`.
- `close_ticket` reads the transitions. When exactly one leads to a status in the category `done`, the tool adds the resolution as a comment and does the transition. With none or more than one, the tool changes nothing and names them. Then call `list_transitions` and `transition_ticket`. When a transition needs a resolution, set `JIRA_RESOLUTION`. When the transition fails after the comment, the error says that the comment was added.

## No JQL injection

`search_tickets` builds the JQL from typed filters. Each text is written as a quoted string with `\` and `"` escaped, so it cannot end the string and add a clause. The project key and the label must match a fixed pattern, and a time is parsed and written again. A control character in a text is refused. A test feeds hostile texts and makes sure that the query without its string literals is always the fixed skeleton. A text in `summary ~` can hold the search syntax of Jira text fields, for example a wildcard. That changes what the text matches and adds no clause.

Jira refuses a search with no restriction. `search_tickets` with `status: any` and no other filter is refused.

## Not verified

No call to a live Jira. The tests use an `httptest` mock that follows the OpenAPI spec of Atlassian. The mock does not prove these points:

- The field `to.statusCategory.key` of a transition. The spec page that was read says only that `to` holds the target status. The server relies on the key `done`.
- The time zone of `created >= ` in JQL. The server writes UTC, and Jira can read the time in the zone of the user of the token.
- The escape of a backslash in JQL. The pages that were read show the escape `\"` only.
- That `POST /rest/api/3/search/jql` is the only search call on your site. The old `/rest/api/3/search` is marked as removed.

## Deployment requirement

The MCP endpoint on `:8080` has no caller authentication. Any caller that reaches the port can call each tool with the vendor credential of this server. The ToolHive proxy of the connector authenticates the daemon in front of it.

1. Apply a NetworkPolicy that admits ingress to this port only from the ToolHive proxy pod of the connector.
2. Do not expose the port through a Service of type `LoadBalancer` or `NodePort`, an Ingress or a port-forward.

The server has no second authentication scheme, by design. The proxy and the NetworkPolicy are the access control.

## Vendor permission

Make a service account for this server and make its API token. Give the account only the project permissions Browse projects, Create issues, Edit issues, Add comments and Transition issues, in the projects that the agents use. The classic scopes are `read:jira-work` and `write:jira-work`. Do not give the account an administrator role.
