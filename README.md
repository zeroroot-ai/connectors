# connectors

This repo holds the MCP servers that Zero Root writes for security tools whose vendor publishes none, and for response actions that no vendor server offers (ADR-0177, ADR-0180). Each server runs behind a Gibson connector. The Gibson catalog pins the image of each server by digest.

## Layout

The repo is one Go module. We chose one module over one module for each tool for three reasons.

1. The shared code is one internal package, `internal/kit`. A separate module for each tool would need a published shared module or a `replace` directive, and the organization forbids `replace`.
2. Each tool still has its own directory, its own `package main` and its own `Dockerfile`. The image of a tool holds only the packages that its binary imports. It is one small static binary.
3. One `go.mod` means one dependency set to scan and to update.

| Path | Content |
|---|---|
| `internal/kit` | Environment, secret type, logger, vendor HTTP client, validation, tool helpers and the streamable HTTP server. |
| `internal/fixloop` | The types of the fix loop that the two Git host servers share. |
| `internal/kit/kittest` | Test helpers: an in-memory MCP client. |
| `<tool>/` | One server: `main.go`, `server.go`, tests, `Dockerfile`, `README.md`. |
| `_template/` | The template of a new server. |
| `scripts/` | `new-server.sh` and the guard `check-servers.sh` with its failing fixture. |

## Start a new server

```sh
make new-server NAME=<tool>
```

This copies `_template/` to `<tool>/`. Then edit `<tool>/server.go`, cite the page of the vendor API reference in a comment, write the tests against an `httptest` mock, and state the vendor permission in `<tool>/README.md`. The image workflow finds the new directory by its `Dockerfile`. It needs no edit.

## How a server runs

- It serves streamable HTTP on `LISTEN_ADDR` (default `:8080`) at `/mcp`. It answers `/healthz` with HTTP 200. The session is stateless.
- It reads each credential and each base URL from its environment. A missing variable stops the start and names the variable, never its value.
- The server has no authentication of its own. The ToolHive proxy of the connector authenticates the daemon in front of it. Do not expose the port in any other way.

## Deployment requirement

The MCP endpoint of each server binds `:8080` with no caller authentication. A caller that reaches the port can call each tool with the vendor credential of the server. The ToolHive proxy of the connector is the only access control.

1. Apply a NetworkPolicy to each server pod that admits ingress to port 8080 only from the ToolHive proxy pod of its connector.
2. Do not expose the port through a Service of type `LoadBalancer` or `NodePort`, an Ingress or a port-forward.

The servers have no second authentication scheme, by design. The README of each server states the same requirement.
- The vendor client sends a request to the fixed base URL only. It builds each path from escaped segments, never follows a redirect, limits the time and the size of a call, and retries only a request that is safe to repeat.
- A log line holds the name of a tool, its outcome and its duration. It never holds an input, an output or a credential.

## Rules

- A server holds no credential in its image. It reads each credential from its environment.
- An input of a model is untrusted. Each tool checks the format and the length of each input. No server builds a shell command, and no server follows a URL from an input.
- Each tool has a test against an `httptest` mock of the vendor API. The mock follows the public API reference of the vendor, and a comment cites the page.
- A tool that changes its target carries the MCP annotations `destructiveHint`, `idempotentHint` and `readOnlyHint`. Its description names the tool that undoes the change, or says that no tool does (ADR-0180).
- A server asks for the least API permission that its tools need. Its README states that permission.

## Commands

| Command | Action |
|---|---|
| `make test` | Run the unit tests. |
| `make check` | Run gofmt, `go vet` and the server guard. |
| `make lint` | Run golangci-lint. |
| `make build` | Compile each server into `bin/`. |

## Servers

| Directory | Tool class | Tools | Issue |
|---|---|---|---|
| `prometheus` | observability | `query_instant`, `query_range`, `list_alerts`, `health_signal` | gibson#1540 |
| `gitlab-fixloop` | git-host | `merge_request_state`, `merge_merge_request`, `revert_merge_request`, `deployment_state` | gibson#1539 |
| `github-fixloop` | git-host | `merge_request_state`, `merge_merge_request`, `revert_merge_request`, `deployment_state` | gibson#1539 |
| `okta` | identity, response | `revoke_user_sessions`, `suspend_user`, `unsuspend_user` | gibson#1548 |
| `entra-id` | identity, response | `revoke_sign_in_sessions`, `disable_account`, `enable_account` | gibson#1549 |
| `crowdstrike` | edr, response | `contain_host`, `lift_containment` | gibson#1545 |
| `defender-endpoint` | edr, response | `isolate_device`, `release_device` | gibson#1546 |
| `sentinelone` | edr, response | `disconnect_from_network`, `reconnect_to_network` | gibson#1547 |
| `qualys` | scanner | `get_host_detections` | gibson#1541 |
| `puppet` | patch | `start_patch_job`, `start_rollback_job`, `get_job` | gibson#1544 |

## Ticket contract

The servers of class `ticket` (`servicenow`, `jira`) use the same tool names, the same argument names and the same result fields. The ticket mirror of the intake agent needs one adapter shape. The types are in `internal/ticket`. A server maps the contract to its vendor API. It adds a vendor tool only when the contract has no tool for the action.

| Tool | Arguments | Result | Undo |
|---|---|---|---|
| `create_ticket` | `title`, `description`, optional `queue`, `priority`, `labels` | `ticket_id`, `number`, `url` | None. Close the ticket with `close_ticket`. |
| `update_ticket` | `ticket_id`, and at least one of `title`, `description`, `priority` | `ticket_id`, `action`, `status` | Call it again with the old value. |
| `add_comment` | `ticket_id`, `body` | `ticket_id`, `action`, `status` | None. The vendor keeps the comment. |
| `close_ticket` | `ticket_id`, `resolution` | `ticket_id`, `action`, `status` | A person reopens the ticket. No tool does. |
| `get_ticket` | `ticket_id` | `ticket_id`, `number`, `url`, `title`, `description`, `status`, `priority`, `closed`, `created_at`, `updated_at` | Not a change. |
| `search_tickets` | optional `queue`, `status` (`open`, `closed`, `any`), `title_contains`, `label`, `created_after`, `limit` | `tickets` (the fields of `get_ticket`), `count` | Not a change. |

- `ticket_id` is the value that the tools of the server take back. It is the key on Jira and the `sys_id` on ServiceNow. `number` is the identifier that a person reads.
- `queue` is the project key on Jira and the `sys_id` of the assignment group on ServiceNow.
- `priority` is `low`, `medium`, `high` or `critical`. The default is `medium`.
- Each search filter is typed. A server builds its query from the filters and never passes free text on as query syntax. Text is quoted and escaped, or refused when it holds a character that the query language cannot escape.
- `description` and `body` are plain text. The server converts it to the format of the vendor.
- A server adds only tools that have no contract name. Jira adds `list_transitions` and `transition_ticket`.

PagerDuty is class `paging` and has its own tools: `trigger_alert`, `acknowledge_alert`, `resolve_alert` and `get_incident`.

## Not built

- Trellix (gibson#1542): the API reference of Trellix did not load from the workstation of the author, and no public source gave the request and response of an isolate call.
- Microsoft Intune (gibson#1543): Microsoft Graph v1.0 has no call that starts a patch for one device, reads its result or rolls it back. The update profiles are in the beta API and they target groups.

## License

Elastic License 2.0. See `LICENSE`.
