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
| `gitlab-fixloop` | git-host | `open_merge_request`, `close_merge_request`, `read_file`, `list_dependency_alerts`, `merge_request_state`, `merge_merge_request`, `revert_merge_request`, `deployment_state` | gibson#1539 |
| `github-fixloop` | git-host | `open_merge_request`, `close_merge_request`, `read_file`, `list_dependency_alerts`, `merge_request_state`, `merge_merge_request`, `revert_merge_request`, `deployment_state` | gibson#1539 |
| `okta` | identity, response | `list_log_events`, `list_users`, `get_user`, `revoke_user_sessions`, `suspend_user`, `unsuspend_user` | gibson#1548, gibson#1436 |
| `entra-id` | identity, response | `revoke_sign_in_sessions`, `disable_account`, `enable_account` | gibson#1549 |
| `crowdstrike` | edr, response | `contain_host`, `lift_containment` | gibson#1545 |
| `defender-endpoint` | edr, response | `isolate_device`, `release_device` | gibson#1546 |
| `sentinelone` | edr, response | `list_threats`, `list_agents`, `disconnect_from_network`, `reconnect_to_network` | gibson#1547, gibson#1440 |
| `qualys` | scanner | `get_host_detections` | gibson#1541 |
| `puppet` | patch | `start_patch_job`, `start_rollback_job`, `get_job` | gibson#1544 |
| `microsoft-intune` | patch | `list_catalog_entries`, `start_patch_deployment`, `get_patch_deployment`, `rollback_patch_deployment`, `resume_patch_deployment`, `remove_patch_deployment` | gibson#1543, gibson#1510 |

## Test images

A test image is no connector. Gibson never pins it in a catalog manifest. The image has the label `ai.zeroroot.image-class=test`, and the release notes list it apart from the servers.

| Directory | What it is | Issue |
|---|---|---|
| `mock-forge` | A stateful mock of the GitHub and Prometheus APIs for the exit test of the fix loop. | gibson#1469 |

## Git host contract

The two servers of class `git-host` (`github-fixloop` and `gitlab-fixloop`) have the same tools, the same arguments and the same result fields. The types are in `internal/fixloop`. `merge_request` is a URL of the host, or `owner/repo#12` on GitHub and `group/project!12` on GitLab.

| Tool | Arguments | Result | Undo |
|---|---|---|---|
| `open_merge_request` | `project`, `branch`, `base_branch`, `title`, `description`, `files` (each `path` with `content`, or `delete: true`) | `merge_request`, `url`, `source_branch`, `target_branch`, `created_at`, `head_sha`, `existing` | `close_merge_request` |
| `close_merge_request` | `merge_request` | `merge_request`, `state`, `closed` | A person reopens it. |
| `list_dependency_alerts` | `project`, `state` (optional, default `open`), `ref` (optional, the default branch only) | `project`, `ref`, `state`, `alerts` (each: `id`, `state`, `package`, `ecosystem`, `purl_type`, `purl`, `version`, `version_source`, `versions`, `version_ambiguous`, `vulnerable_version_range`, `first_patched_version`, `advisory_ids`, `verifiable`, `severity`, `manifest_path`, `url`), `truncated` | Not a change. |
| `read_file` | `project`, `ref`, `path` | `found`, `content`, `sha` (the blob sha), `size`, `path`, `ref` | Not a change. |
| `merge_request_state` | `merge_request` | `state`, `pipeline_green`, `pipeline_status`, `reason`, `head_sha`, `source_branch`, `created_at`, `merge_commit`, `changed_files`, `changed_files_truncated`, `changed_files_error` | Not a change. |
| `merge_merge_request` | `merge_request`, `expected_head_sha` (required) and, on GitHub, optional `merge_method` | `merged`, `state`, `reason`, `head_sha`, `merge_commit` | `revert_merge_request` |
| `revert_merge_request` | `merge_request` | `revert_merge_request`, `state`, `merged` | `revert_merge_request` on the revert |
| `deployment_state` | `merge_request` | `deployed`, `application`, `at`, `restart` | Not a change. |

Rules of the contract:

- `open_merge_request` makes the source branch from the head of `base_branch`, and puts all files in one commit. It never writes to the base branch and never merges.
- When the source branch exists, the tool accepts it only when its head is exactly one commit beyond the base branch, and that commit changes exactly the files of the input with the git blob sha of the input content, read at that commit, and with the file mode that the tool writes (a delete is a removed file). GitHub: the tool reads the tree of the commit and requires the mode `100644` and the type `blob` for each path. GitLab: it checks `a_mode` and `b_mode` of each diff. Then it returns the open merge request of the branch, or opens one, and makes no commit. In every other case it refuses with the reason. It never builds on a branch that holds a foreign commit.
- `head_sha` of `open_merge_request` is the commit that the tool made, or checked on a resume. The tool refuses when the head of the merge request is another commit. `head_sha` of `merge_request_state` is the head now. A caller passes it as `expected_head_sha` to `merge_merge_request`. It is required. The tool merges nothing when the head is another commit, and the reason names `expected_head_sha` and both shas. It also sends the head sha to the host.
- `changed_files` is a list of `{path, change, previous_path, blob_sha, mode}`. `change` is `added`, `modified`, `deleted`, `renamed` or `mode_changed`. `mode` is the file mode at the head. GitLab gives it, and GitHub does not. `previous_path` is set for a rename. `blob_sha` is the git blob sha of the file at the head, and it is empty for a deleted file. The list has at most 100 files. `changed_files_truncated` is true when the merge request has more. When the host does not give the list, `changed_files` is empty and `changed_files_error` holds the cause, so an empty list with an error means unknown. An agent computes the git blob sha of its expected content (`SHA-1` of `blob <length>`, a zero byte and the content) and compares it, so no content travels.
- Only `merge_request_state` fills `changed_files`. The merge tools do not read it.

## Ticket contract

The servers of class `ticket` (`servicenow`, `jira`) use the same tool names, the same argument names and the same result fields. The ticket mirror of the intake agent needs one adapter shape. The types are in `internal/ticket`. A server maps the contract to its vendor API. It adds a vendor tool only when the contract has no tool for the action.

| Tool | Arguments | Result | Undo |
|---|---|---|---|
| `create_ticket` | `title`, `description`, optional `queue`, `priority`, `labels`, `dedup_key` | `ticket_id`, `number`, `url`, `existing` | `close_ticket` |
| `update_ticket` | `ticket_id`, and at least one of `title`, `description`, `priority` | `ticket_id`, `action`, `status` | Call it again with the old value. |
| `add_comment` | `ticket_id`, `body` | `ticket_id`, `action`, `status` | None. The vendor keeps the comment. |
| `close_ticket` | `ticket_id`, `resolution` | `ticket_id`, `action`, `status` | No tool of the contract. A person reopens the ticket. Jira also has `transition_ticket`. |
| `get_ticket` | `ticket_id` | `ticket_id`, `number`, `url`, `title`, `description`, `status`, `priority`, `closed`, `created_at`, `updated_at` | Not a change. |
| `search_tickets` | optional `queue`, `status` (`open`, `closed`, `any`), `title_contains`, `label`, `dedup_key`, `created_after`, `limit` | `tickets` (the fields of `get_ticket`), `count` | Not a change. |

- `ticket_id` is the value that the tools of the server take back. It is the key on Jira and the `sys_id` on ServiceNow. `number` is the identifier that a person reads.
- `queue` is the project key on Jira and the `sys_id` of the assignment group on ServiceNow.
- `priority` is `low`, `medium`, `high` or `critical`. The default is `medium`.
- Each search filter is typed. A server builds its query from the filters and never passes free text on as query syntax. Each server states how it makes a text safe. It quotes and escapes the text, or it refuses a text with a character that the query language cannot escape.
- `dedup_key` makes `create_ticket` safe to repeat. The server searches for a ticket that holds the key, and stores the key in the new ticket (the field `correlation_id` on ServiceNow, a label on Jira). A second call with the same key, for example a retry after a timeout, returns the first ticket with `existing: true`. The search and the create are two calls, so two calls at the same time can still make two tickets. A server does not repeat a create call by itself, because a repeat could make a second ticket.
- `search_tickets` with `dedup_key` finds the tickets that `create_ticket` made with this key. Jira searches the label of the key. ServiceNow matches `correlation_id` exactly. ServiceNow has no labels, so this is the way to find a ticket there. The result holds at most `limit` tickets, oldest first, and `count` is the number of tickets in the result. The status defaults to `any`, so a ticket that someone closed is found. The other filters still apply. The key has the same format as in `create_ticket`, so it cannot add a condition to the query. ServiceNow also refuses `^` and `javascript:` in the key.
- `description` and `body` are plain text. The server converts it to the format of the vendor.
- A server adds only tools that have no contract name. Jira adds `list_transitions` and `transition_ticket`.

PagerDuty is class `paging` and has its own tools: `trigger_alert`, `acknowledge_alert`, `resolve_alert`, `get_incident` and `find_incidents`.


## Not built

- Trellix isolate and release (gibson#1561): the isolate call is documented, and the release call is not confirmed. The `trellix` server has the read tools only. See its README.

## License

Elastic License 2.0. See `LICENSE`.
