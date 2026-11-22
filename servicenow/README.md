# servicenow

MCP server for incidents in ServiceNow, on the Table API. Class `ticket`. The tool names, the arguments and the results follow the ticket contract of the repo (see `README.md` of the repo, section "Ticket contract"). It serves streamable HTTP on `:8080` at `/mcp` and answers `/healthz`.

The ServiceNow MCP server of the vendor runs on the instance of the customer. Its tool list is not public, so an argument name cannot be checked. This server uses the documented Table API with fixed tool names.

## Environment

| Variable | Meaning |
|---|---|
| `SERVICENOW_INSTANCE_URL` | Required. The address of the instance, for example `https://example.service-now.com`. It must use https. |
| `SERVICENOW_USER` | Required. The user name of a service account. |
| `SERVICENOW_PASSWORD` | Required. Its password. The server sends HTTP basic authentication. |
| `SERVICENOW_CLOSE_CODE` | Optional. The `close_code` that `close_ticket` sets. The default is `Solved (Permanently)`. The valid values depend on the instance. |
| `SERVICENOW_TIMEOUT` | Optional. Time limit of one vendor call. The default is `20s`. |
| `LISTEN_ADDR` | Optional. The default is `:8080`. |

## Tools

Reference: https://www.servicenow.com/docs/r/api-reference/rest-apis/c_TableAPI.html. All calls are on the table `incident`.

| Tool | Vendor call | Undo | Annotations |
|---|---|---|---|
| `create_ticket` | `GET /api/now/table/incident` (look for `dedup_key`), then `POST /api/now/table/incident` | `close_ticket` | `destructiveHint: false`, `idempotentHint: false` |
| `update_ticket` | `PATCH /api/now/table/incident/{sys_id}` | `update_ticket` with the old value | `destructiveHint: false`, `idempotentHint: true` |
| `add_comment` | `PATCH /api/now/table/incident/{sys_id}` with `work_notes` | None. ServiceNow keeps the note. | `destructiveHint: true`, `idempotentHint: false` |
| `close_ticket` | `PATCH /api/now/table/incident/{sys_id}` with `state`, `close_code`, `close_notes` | None. No tool of this server reopens an incident. A person does. | `destructiveHint: true`, `idempotentHint: true` |
| `get_ticket` | `GET /api/now/table/incident/{sys_id}` | Not a change. | `readOnlyHint: true` |
| `search_tickets` | `GET /api/now/table/incident` with `sysparm_query` | Not a change. | `readOnlyHint: true` |

How the contract maps to ServiceNow:

- `ticket_id` is the `sys_id` (32 hex characters). `number` is the incident number. A login, a number or a path is refused.
- `queue` is the `sys_id` of the assignment group.
- `priority` sets `urgency` and `impact`, because ServiceNow computes the priority from them: `critical` 1 and 1, `high` 1 and 2, `medium` 2 and 2, `low` 3 and 3. The read tools show the priority field: 1 is `critical`, 2 `high`, 3 `medium`, 4 and 5 `low`.
- `add_comment` writes a work note. The caller of the incident does not see it.
- `dedup_key` is stored in `correlation_id`. Before a create, the tool reads the oldest incident with that `correlation_id`, in any state. When one exists, the tool returns it with `existing: true` and makes no new incident. The create call is never repeated by the client, so a timeout cannot make two incidents. The search and the create are two calls, so two calls at the same time can still make two. The key has a fixed pattern of at most 100 characters, so it cannot add a condition to the query.
- `search_tickets` with `dedup_key` adds the condition `correlation_id=<key>`, an exact match, and sorts by `opened_at` with the oldest incident first. The status defaults to `any`. The key has the contract format, and the tool also refuses `^` and `javascript:` in it. This is the way to find an incident that a client made, because ServiceNow has no labels.
- `close_ticket` sets state 7 with the close code of the server and the resolution as the close notes. ServiceNow can answer 200 and keep the old state, for example when a business rule refuses the change. The tool then returns an error. A close from some states can need a resolve first. The tool shows the error of ServiceNow.
- `status` is the state number. `closed` is true for state 6 and higher. `search_tickets` uses the same rule: `status: open` is `state<6` and `status: closed` is `state>=6`. It does not use the field `active`, so a get and a search agree. The page https://www.servicenow.com/docs/r/7LhbZn3yz3Q58qhjZoU9iQ/nCIOdm4VSGlySPh~AEaryw gives only 6 as Resolved and 7 as Closed.
- An incident has no labels. A call with `labels`, or a search with `label`, is refused. It is not dropped without a notice.

## Close code

`close_code` must be one of the choices of the instance. The choices are in the table `sys_choice` (name `incident`, element `close_code`). Read them with `GET /api/now/table/sys_choice?sysparm_query=name=incident^element=close_code&sysparm_fields=value,label`. The default is `Solved (Permanently)`. Set `SERVICENOW_CLOSE_CODE` to a value that the instance lists. A value that the instance does not list is refused by ServiceNow, and the tool shows the error.

## Text that ServiceNow renders

ServiceNow shows the text between `[code]` and `[/code]` as HTML in a journal field and in some fields of a form. A text from a finding could put markup in the view of a person. The tool writes `[ code ]` and `[ /code ]` for each tag, in any case, in the title, the description, the work note and the close notes.

## No query injection

`search_tickets` builds `sysparm_query` from typed filters. The character `^` separates conditions in the encoded query, and the Table API documents no escape for it. So a `title_contains` with `^` is refused. A value that starts with `javascript:` is run as a script in an encoded query, so a `title_contains` with `javascript:` at any place is refused. The ids are 32 hex characters. The status is one of three values. The time is parsed as RFC 3339 and written again. A test feeds injection texts and makes sure that none reaches ServiceNow.

## Security incidents not built

The table `sn_si_incident` (Security Incident Response) is not built. The Table API applies to it, but no public page states its fields, its state values or its roles. The roles that the vendor names are `sn_si.read`, `sn_si.basic`, `sn_si.analyst` and `sn_si.integration_user`. A tool that guesses a field name fails without a notice, so none is written. Add it when a tenant confirms the fields.

## Not verified

No call to a live ServiceNow. The tests use an `httptest` mock that follows the Table API reference. The mock does not prove these points:

- The format of an error body. The tool shows `error.message` and `error.detail` when they exist, and the plain HTTP status when they do not.
- The address `incident.do?sys_id=...` of the web interface. The server builds it from the instance address.
- That `opened_at>=` in an encoded query is read as UTC on every instance.
- That a close from the state New is allowed on every instance.

## Deployment requirement

The MCP endpoint on `:8080` has no caller authentication. Any caller that reaches the port can call each tool with the vendor credential of this server. The ToolHive proxy of the connector authenticates the daemon in front of it.

1. Apply a NetworkPolicy that admits ingress to this port only from the ToolHive proxy pod of the connector.
2. Do not expose the port through a Service of type `LoadBalancer` or `NodePort`, an Ingress or a port-forward.

The server has no second authentication scheme, by design. The proxy and the NetworkPolicy are the access control.

## Vendor permission

Make a service account for this server. Give it the role that reads and writes incidents (`itil`), and limit it to the assignment groups that the agents use. Do not give it an administrator role. The Table API applies the access control lists of the table. The vendor page does not name the role for the Table API, so check the role on your instance.
