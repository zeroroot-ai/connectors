# gitlab-fixloop

MCP server for the fix loop on GitLab (tool class: git-host). The vendor server of GitLab has none of these tools. The Gibson daemon calls `merge_request_state`, `deployment_state` and `revert_merge_request` to watch a fix (`internal/engine/fixloop/connector.go` in zeroroot-ai/gibson). The fix agent calls `merge_merge_request`. The server serves streamable HTTP on `:8080` at `/mcp` and answers `/healthz`.

## Environment

| Variable | Meaning |
|---|---|
| `GITLAB_BASE_URL` | Required. The address of GitLab, for example `https://gitlab.com`. The server adds `/api/v4`. |
| `GITLAB_TOKEN` | Required. A project, group or personal access token. The server sends it as `PRIVATE-TOKEN`. |
| `GITLAB_ENVIRONMENT` | Required. The name of the GitLab environment that `deployment_state` reads, for example `production`. |
| `GITLAB_TIMEOUT` | Optional. Time limit of one vendor call. The default is `20s`. |
| `LISTEN_ADDR` | Optional. The default is `:8080`. |

## Tools

References: https://docs.gitlab.com/api/merge_requests/, https://docs.gitlab.com/api/commits/#revert-a-commit, https://docs.gitlab.com/api/branches/, https://docs.gitlab.com/api/repositories/, https://docs.gitlab.com/api/deployments/, https://docs.gitlab.com/api/projects/ and https://docs.gitlab.com/api/status_checks/.

The argument `merge_request` is the URL of the merge request on the server of `GITLAB_BASE_URL`, or the short form `group/project!12`. A URL on another host is refused.

| Tool | Vendor calls | Undo | Annotations |
|---|---|---|---|
| `merge_request_state` | `GET /projects/:id/merge_requests/:iid`. For an open merge request with a green pipeline also `GET /projects/:id` (the merge method) unless the merge request squashes, and `GET .../status_checks` when the status is `status_checks_must_pass` | Not needed. It reads. | `readOnlyHint: true` |
| `merge_merge_request` | the reads above, then `PUT .../merge` with `sha` | `revert_merge_request` | `destructiveHint: false`, `idempotentHint: true` |
| `revert_merge_request` | list and create merge requests, create a branch, `POST /repository/commits/:sha/revert`, then the merge call above | `revert_merge_request` on the revert merge request | `destructiveHint: false`, `idempotentHint: true` |
| `open_merge_request` | `GET /projects/:id/merge_requests`, `HEAD /projects/:id/repository/files/:path` for each file, `POST /projects/:id/repository/commits` with `start_branch`, then `POST /projects/:id/merge_requests` | Close the merge request. No tool of this server closes it. | `destructiveHint: false`, `idempotentHint: true` |
| `deployment_state` | `GET` the merge request, `GET /projects/:id/deployments`, `GET /repository/merge_base` | Not needed. It reads. | `readOnlyHint: true` |

### Mapping to the daemon shape

The input and the result follow `fixloop/connector.go`. Where GitLab differs, the daemon shape wins.

| Field of the daemon | Source on GitLab |
|---|---|
| `state` = `open` | `state` is `opened` or `locked`. |
| `state` = `merged`, `closed` | `state` is `merged` or `closed`. |
| `state` = `refused` | GitLab has no such state. See the next section. |
| `pipeline_green` | `head_pipeline.status` is `success`, and `head_pipeline.sha` is the head sha of the merge request. A pipeline with an empty sha does not count. The result also holds `pipeline_status`, the raw status, or `none`. When `detailed_merge_status` is `status_checks_must_pass`, the tool also reads the external status checks. A `failed` check makes `pipeline_status` `failed`. A check that has not passed makes it `pending`. |
| `at` | `merged_at`, `closed_at` or `updated_at`. |
| `source_branch` | `source_branch` of the merge request. The daemon links the merge request of a fix job to a finding only when this is the branch of the job. The field is absent when the branch is in a fork: `source_project_id` is not `target_project_id`, or GitLab gives no `source_project_id` (a deleted fork). A fork can hold a branch with any name, so the daemon then refuses the link. |
| `source_branch_reason` | Set only when `source_branch` is absent for a fork: `the source branch is in a fork, not in the project of the merge request`. |
| `created_at` | `created_at` of the merge request, in RFC 3339 in UTC. The daemon links the merge request only when it was opened at or after the claim of the job. With no value, the field is absent and the daemon refuses the link. |
| `merge_commit` | `merge_commit_sha`, or `squash_commit_sha` for a squash merge. |
| `deployment_state.deployed` | A deployment with status `success` to `GITLAB_ENVIRONMENT` holds the merge commit. |
| `deployment_state.application` | The path of the project. The tenant keys its health query by this value. |
| `deployment_state.at` | `finished_at` of the deployment, or `updated_at` when GitLab gives none. |
| `deployment_state.restart` | `needed` is always false. GitLab has no restart. |

### Merge rule

`merge_merge_request` merges only when all of these hold.

1. The merge request is open.
2. The head pipeline has the status `success` and ran for the current head commit.
3. `detailed_merge_status` is `mergeable`.
4. The `sha` argument, when given, is the head commit.

The tool sends the head sha that it checked in the merge call. GitLab refuses the merge with HTTP 409 when a push changed the head after the check. The result then has `merged: false` and the state `open`.

### Refused

A rule of the project can block a merge: a missing approval, an open thread, a draft, a conflict. GitLab shows the rule in `detailed_merge_status`. When the head pipeline succeeded and the status is not `mergeable` and not a status that passes with no person (`checking`, `unchecked`, `preparing`, `approvals_syncing`, `ci_still_running`, `ci_must_pass`, `merge_time`), both `merge_request_state` and `merge_merge_request` return the state `refused`. The reason is `GitLab does not allow the merge: <detailed_merge_status>`, for example `GitLab does not allow the merge: not_approved`. `merge_merge_request` then does not call the merge.

`merge_time` is "May not be merged until after the specified time". The time passes with no person, so the state stays `open`. `status_checks_must_pass` is "All status checks must pass before merge". Some GitLab versions answer `external_status_checks`. The tool reads the checks of the merge request for both. While a check is pending, the state is `open` and `pipeline_status` is `pending`. When every check passed and GitLab still shows the status, the state is `refused`.

A project with `merge_method` `ff` merges by fast-forward. Without a squash, that merge leaves no merge commit and no squash commit, so `revert_merge_request` cannot undo it. GitLab picks the merge method from the project, and the merge call has no method argument. So both tools return `refused` with the fixed reason `GitLab does not allow the merge: the project merges by fast-forward and this merge request does not squash, so the merge leaves no commit that the revert tool can revert`. The tool reads `squash_on_merge` of the merge request, or `squash` on a GitLab that has no `squash_on_merge`.

The server keeps no memory of a refusal. It reads the state from GitLab on each call. When a person removes the block, the next call returns `open`. Each replica of the server, and a server after a restart, gives the same answer.

A refusal of the merge call (HTTP 403, 405, 406 or 422) returns `refused` in the result of that call. The reason is `GitLab refused the merge (HTTP <status>): <message of GitLab>`. The reason stays the same while the cause stays, so the daemon can see a repeat. GitLab does not show every cause of such a refusal in a read, so the next `merge_request_state` can return `open`.

A merge request from a fork, or from a fork that a person deleted, is never merged or reverted. The tool sees a fork when `source_project_id` is not `target_project_id`, or GitLab gives no `source_project_id`. A fork can hold a branch with any name, the name of a job branch too. `merge_merge_request` and `revert_merge_request` return `refused` with the fixed reason `the source branch is in a fork, not in the project of the merge request`. They change nothing on the host. A merge request that is already merged stays `merged` in the result of `merge_merge_request`.

### Revert

`revert_merge_request` follows these steps.

1. It reads the merge request. The merge request must be merged and must have a merge commit or a squash commit.
2. It looks for a merge request from the branch `zeroroot/revert-mr<number>-<commit>`. It finds the work of an earlier call, so the same call twice opens one revert merge request. When a person closed that merge request without a merge, it tries the branch with the suffix `-2`, then `-3`. After three closed revert merge requests, the tool returns an error and a person reverts by hand. A closed revert never stops the loop without an answer.
3. It creates that branch from the target branch, and reverts the merge commit onto it with the commits API. It opens a merge request from the branch.
4. When the branch exists already with one commit after the target branch, the tool keeps it only when that commit reverts the merge commit. The commit must have one parent, and that parent must hold the merge commit. Each hunk of each file must equal the hunk of the compare of the merge commit to its first parent: the old and new line ranges, and the added and removed lines. So a commit that makes the same edit in another place of a file is refused. Context lines can differ. When the target branch moved the lines of the edit, the ranges differ and the tool refuses the branch. Delete the branch, and the next call makes a new one. Otherwise a person made the branch, and the tool refuses with an error. A branch with more than one such commit is refused too.
5. It merges the revert merge request by the merge rule above. When the pipeline has not finished, it returns the state of the revert merge request and `merged: false`. Call the tool again later to merge it.

Limits:

- A fast-forward merge has no merge commit and no squash commit. The tool refuses it.
- GitLab refuses the revert (HTTP 400) when the revert conflicts with later commits. The tool shows the error. A person must revert by hand.
- A project without a pipeline has no head pipeline. The tool opens the revert merge request and never merges it.
- The tool does not wait for the pipeline. The caller calls it again.

## open_merge_request

The input is `repo` (the project path, for example `group/project`), `source_branch`, `target_branch`, `title`, `body` and `changes`. A change is a `path` with `content` (the new text of the file) or `delete: true`. One call makes one commit with all changes, on the new source branch, from the target branch, and opens the merge request. The result holds `merge_request` (`group/project!12`), `url`, `source_branch`, `target_branch` and `created_at`.

GitLab has no commit action that writes a file whether it exists or not. For each file with content, the tool sends `HEAD` for the path on the target branch. It uses the action `update` when GitLab answers 200 and `create` when it answers 404. A change with `delete: true` uses the action `delete`.

The tool never writes to the target branch and never merges. It refuses a source branch that exists, unless an open merge request goes from that branch into the target branch. It then returns that merge request with `existing: true` and writes nothing. It does not compare the content of that branch with the input. A person undoes the action by closing the merge request. No tool of this server closes it. The merge request asks GitLab to remove the source branch after a merge.

The input has these limits: 100 files, 1 MiB for one file, 8 MiB for all files, a title of 256 bytes, a branch name of plain characters, and a path with no `..`, no empty folder name and no `.git` folder.

References: https://docs.gitlab.com/api/repository_files/, https://docs.gitlab.com/api/commits/ and https://docs.gitlab.com/api/merge_requests/.

## Deployment requirement

The MCP endpoint on `:8080` has no caller authentication. Any caller that reaches the port can call each tool with the vendor credential of this server. The ToolHive proxy of the connector authenticates the daemon in front of it.

1. Apply a NetworkPolicy that admits ingress to this port only from the ToolHive proxy pod of the connector.
2. Do not expose the port through a Service of type `LoadBalancer` or `NodePort`, an Ingress or a port-forward.

The server has no second authentication scheme, by design. The proxy and the NetworkPolicy are the access control.

## Vendor permission

The scope `api` of the access token. GitLab has no finer scope for a merge, a branch, a commit or a merge request write. Use a project access token with the role Developer, or Maintainer when the protected target branch allows only Maintainers to merge. Give the token to the projects that the agents fix and to no other project. Do not use a personal token of an administrator.

## Not verified

No call to a live GitLab. The tests run against an `httptest` mock that follows the API reference above. The mock does not prove these points:

- The text of a refusal on a real GitLab. The tool shows the message that GitLab sends.
- That GitLab reverts a merge commit with the commits API on every version. The API reference does not state it. The tool shows the error when GitLab refuses.
- That the deployments API of a given GitLab version has `finished_at`. The tool uses `updated_at` when it is missing.
- That the compare API marks a cut diff with `too_large` or `collapsed` on every version. A cut diff that GitLab does not mark can make the check of an existing revert branch give a wrong answer.
- The name `external_status_checks`. The API reference names only `status_checks_must_pass`.
