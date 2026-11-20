# github-fixloop

MCP server for the fix loop on GitHub (tool class: git-host). The vendor server of GitHub has none of these tools. The Gibson daemon calls `merge_request_state`, `deployment_state` and `revert_merge_request` to watch a fix (`internal/engine/fixloop/connector.go` in zeroroot-ai/gibson). The fix agent calls `merge_merge_request`. The server serves streamable HTTP on `:8080` at `/mcp` and answers `/healthz`.

The tools have the same names, arguments and results as the tools of `gitlab-fixloop`. A pull request is the merge request.

## Environment

| Variable | Meaning |
|---|---|
| `GITHUB_TOKEN` | Required. A fine-grained token, or the token of a GitHub App installation. The server sends it as `Authorization: Bearer`. |
| `GITHUB_ENVIRONMENT` | Required. The name of the GitHub environment that `deployment_state` reads, for example `production`. |
| `GITHUB_API_URL` | Optional. The default is `https://api.github.com`. For GitHub Enterprise Server, use `https://<host>/api/v3`. |
| `GITHUB_TIMEOUT` | Optional. Time limit of one vendor call. The default is `20s`. |
| `LISTEN_ADDR` | Optional. The default is `:8080`. |

## Tools

References: https://docs.github.com/en/rest/pulls/pulls, https://docs.github.com/en/rest/checks/runs, https://docs.github.com/en/rest/commits/statuses, https://docs.github.com/en/rest/git/commits, https://docs.github.com/en/rest/git/refs, https://docs.github.com/en/rest/deployments/deployments, https://docs.github.com/en/rest/deployments/statuses and https://docs.github.com/en/rest/commits/commits#compare-two-commits.

The argument `merge_request` is the URL of the pull request on the host of the server, or the short form `owner/repo#12`. A URL on another host is refused.

| Tool | Vendor calls | Undo | Annotations |
|---|---|---|---|
| `merge_request_state` | `GET /repos/{o}/{r}/pulls/{n}`, check runs and combined status of the head commit | Not needed. It reads. | `readOnlyHint: true` |
| `merge_merge_request` | the read above, then `PUT /repos/{o}/{r}/pulls/{n}/merge` with `sha` | `revert_merge_request` | `destructiveHint: false`, `idempotentHint: true` |
| `revert_merge_request` | git data API and pull request calls, then the merge call above | `revert_merge_request` on the revert pull request | `destructiveHint: false`, `idempotentHint: true` |
| `deployment_state` | `GET` the pull request, `GET /deployments`, `GET /deployments/{id}/statuses`, `GET /compare/{base}...{head}` | Not needed. It reads. | `readOnlyHint: true` |

### Mapping to the daemon shape

The input and the result follow `fixloop/connector.go`. Where GitHub differs, the daemon shape wins.

| Field of the daemon | Source on GitHub |
|---|---|
| `state` = `open`, `merged`, `closed` | `state` is `open`. `state` is `closed` and `merged` is true. `state` is `closed` and `merged` is false. |
| `state` = `refused` | GitHub has no such state. See the next section. |
| `pipeline_green` | Every check run of the head commit is complete with the conclusion `success`, `neutral` or `skipped`. The combined commit status is `success`, or the commit has no status. The commit has at least one check run or one status. |
| `pipeline_status` | `success`, `pending`, `failed` or `none`. |
| `at` | `merged_at`, `closed_at` or `updated_at`. |
| `merge_commit` | `merge_commit_sha` of the pull request. |
| `deployment_state.deployed` | A deployment to `GITHUB_ENVIRONMENT` holds the merge commit and has a status `success`. Its newest status is not `failure` or `error`. |
| `deployment_state.application` | `owner/repo`. The tenant keys its health query by this value. |
| `deployment_state.at` | The time of the first `success` status of the deployment. |
| `deployment_state.restart` | `needed` is always false. GitHub has no restart. |

### Merge rule

`merge_merge_request` merges only when all of these hold.

1. The pull request is open.
2. The head commit has `pipeline_green`.
3. `mergeable_state` is `clean` or `has_hooks`. When GitHub has not computed it yet, the tool reads the pull request again after one second.
4. The `sha` argument, when given, is the head commit.

The tool sends the head sha that it checked in the merge call. GitHub refuses the merge with HTTP 409 when a push changed the head after the check. The result then has `merged: false` and the state `open`. The optional `merge_method` is `merge` (the default) or `squash`. The tool does not offer a rebase merge, because `revert_merge_request` cannot revert one.

### Refused

A rule of the repository can block a merge: a missing review, a conflict, a draft, a branch that is behind. The tool then returns the state `refused` with `mergeable_state` as the reason, and it does not call the merge. A refusal of the merge call (HTTP 403, 405 or 422) has the same result. `merge_request_state` returns `refused` for that pull request while its head commit stays the same. The server keeps this memory in its process. After a restart, the next call of `merge_merge_request` sets it again.

### Revert

GitHub has no revert call in the REST API. `revert_merge_request` builds the revert with the git data API.

1. It reads the pull request. The pull request must be merged.
2. It looks for a pull request from the branch `zeroroot/revert-pr-<number>-<commit>`. It finds the work of an earlier call, so the same call twice opens one revert pull request.
3. It reads the base branch. The base branch must end at the merge commit.
4. It makes a commit with the tree of the first parent of the merge commit and the merge commit as its parent. It creates the branch at that commit and opens a pull request from the branch.
5. It merges the revert pull request by the merge rule above. When the checks have not finished, it returns the state of the revert pull request and `merged: false`. Call the tool again later to merge it.

Limits:

- The revert is exact only while no commit came after the merge. The tree of the parent of the merge commit holds no later work. When the base branch has later commits, the tool refuses and makes nothing. A person must revert by hand. This tool does not do a three-way merge, because the git data API has none.
- The tool reverts a merge commit and a squash commit. It does not revert a rebase merge, because the merge commit of a rebase is the last of several commits.
- A repository with no check run and no commit status has no `pipeline_green`. The tool opens the revert pull request and never merges it.
- The tool does not wait for the checks. The caller calls it again.
- The commit has the author of the token. GitHub shows it as unverified unless the token belongs to a GitHub App that signs commits.

## Vendor permission

Use a fine-grained token or a GitHub App installation, for the repositories that the agents fix and no others. It needs these repository permissions.

| Permission | Level | Reason |
|---|---|---|
| Metadata | Read | Required by GitHub. |
| Pull requests | Read and write | Read, open and merge a pull request. |
| Contents | Read and write | Merge a pull request. Create the revert commit and branch. |
| Checks | Read | Read the check runs. |
| Commit statuses | Read | Read the combined status. |
| Deployments | Read | Read the deployments and their statuses. |

Branch protection of the repository still applies to the token. A rule that needs a review blocks the merge, and the tool reports it as `refused`.

## Not verified

No call to a live GitHub. The tests run against an `httptest` mock that follows the API reference above. The mock does not prove these points:

- The text of a refusal on a real GitHub. The tool shows the message that GitHub sends.
- That `mergeable_state` is `clean` for every mergeable pull request. A repository with a merge queue or a ruleset can answer differently.
- That a tree of a merge commit parent gives the same tree as a Git revert on a real repository. It does when the base branch ends at the merge commit.
