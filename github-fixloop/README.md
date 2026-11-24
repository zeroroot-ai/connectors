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

References: https://docs.github.com/en/rest/pulls/pulls, https://docs.github.com/en/rest/checks/runs, https://docs.github.com/en/rest/commits/statuses, https://docs.github.com/en/rest/git/commits, https://docs.github.com/en/rest/git/refs, https://docs.github.com/en/rest/deployments/deployments, https://docs.github.com/en/rest/deployments/statuses, https://docs.github.com/en/rest/commits/commits#compare-two-commits, https://docs.github.com/en/rest/pulls/pulls#list-commits-on-a-pull-request, https://docs.github.com/en/rest/commits/commits#list-pull-requests-associated-with-a-commit, https://docs.github.com/en/rest/repos/repos#get-a-repository, https://docs.github.com/en/rest/branches/branches#get-a-branch and https://docs.github.com/en/rest/repos/rules#get-rules-for-a-branch.

The argument `merge_request` is the URL of the pull request on the host of the server, or the short form `owner/repo#12`. A URL on another host is refused.

| Tool | Vendor calls | Undo | Annotations |
|---|---|---|---|
| `merge_request_state` | `GET /repos/{o}/{r}/pulls/{n}`, check runs and combined status of the head commit. For an open pull request also `GET /repos/{o}/{r}/branches/{base}`, `GET /repos/{o}/{r}/rules/branches/{base}` and `GET /repos/{o}/{r}` | Not needed. It reads. | `readOnlyHint: true` |
| `merge_merge_request` | the reads above, then `PUT /repos/{o}/{r}/pulls/{n}/merge` with `sha` and `merge_method` | `revert_merge_request` | `destructiveHint: false`, `idempotentHint: true` |
| `revert_merge_request` | git data API and pull request calls, then the merge call above | `revert_merge_request` on the revert pull request | `destructiveHint: false`, `idempotentHint: true` |
| `open_merge_request` | `GET` the source and target references and the target commit, `POST /repos/{o}/{r}/git/blobs`, `/git/trees`, `/git/commits`, `/git/refs`, then `POST /repos/{o}/{r}/pulls` | `close_merge_request` | `destructiveHint: false`, `idempotentHint: true` |
| `close_merge_request` | `GET /repos/{o}/{r}/pulls/{n}`, then, when `comment` is set, `GET /repos/{o}/{r}/issues/{n}/comments` and, when no comment has the same body, `POST /repos/{o}/{r}/issues/{n}/comments`, then `PATCH /repos/{o}/{r}/pulls/{n}` with `state: closed` | A person reopens it. No tool reopens it. | `destructiveHint: false`, `idempotentHint: true` |
| `list_tree` | `GET /repos/{o}/{r}` (default branch only), `GET /repos/{o}/{r}/git/trees/{sha or ref}` for each folder of `path`, then the same with `recursive=1` | Not a change. | `readOnlyHint: true` |
| `list_dependency_alerts` | `GET /repos/{o}/{r}`, `GET /repos/{o}/{r}/dependabot/alerts`, and `GET /repos/{o}/{r}/contents/{manifest}?ref=` | Not a change. | `readOnlyHint: true` |
| `read_file` | `GET /repos/{o}/{r}/contents/{path}?ref=` and, on a 404, `GET /repos/{o}/{r}` | Not a change. | `readOnlyHint: true` |
| `deployment_state` | `GET` the pull request, `GET /deployments`, `GET /deployments/{id}/statuses`, `GET /compare/{base}...{head}` | Not needed. It reads. | `readOnlyHint: true` |

### Mapping to the daemon shape

The input and the result follow `fixloop/connector.go`. Where GitHub differs, the daemon shape wins.

| Field of the daemon | Source on GitHub |
|---|---|
| `state` = `open`, `merged`, `closed` | `state` is `open`. `state` is `closed` and `merged` is true. `state` is `closed` and `merged` is false. |
| `state` = `refused` | GitHub has no such state. See the next section. |
| `pipeline_green` | Every check run of the head commit is complete with the conclusion `success`, `neutral` or `skipped`. The combined commit status is `success`, or the commit has no status. The commit has at least one check run or one status. For an open pull request, each check that a rule of the base branch requires (branch protection or a ruleset) has a check run or a status with its name. A required check that has not reported makes `pipeline_status` `pending`, and `reason` names each such check: `these required checks have not reported: <name>, <name>`. The tool reads at most 1000 check runs (10 pages of 100) and 1000 statuses (10 pages of 100). A commit with more check runs or more statuses gets a tool error, never `pipeline_green`. |
| `pipeline_status` | `success`, `pending`, `failed` or `none`. |
| `at` | `merged_at`, `closed_at` or `updated_at`. |
| `source_branch` | `head.ref` of the pull request. The daemon links the merge request of a fix job to a finding only when this is the branch of the job. The field is absent when the branch is in a fork: `head.repo.full_name` is not `base.repo.full_name`, or `head.repo` is null (a deleted fork). A fork can hold a branch with any name, so the daemon then refuses the link. |
| `source_branch_reason` | Set only when `source_branch` is absent for a fork: `the source branch is in a fork, not in the project of the merge request`. |
| `created_at` | `created_at` of the pull request, in RFC 3339 in UTC. The daemon links the merge request only when it was opened at or after the claim of the job. With no value, the field is absent and the daemon refuses the link. |
| `merge_commit` | `merge_commit_sha` of the pull request. |
| `deployment_state.deployed` | A deployment to `GITHUB_ENVIRONMENT` holds the merge commit and has a status `success`. Its newest status is not `failure` or `error`. |
| `deployment_state.application` | `owner/repo`. The tenant keys its health query by this value. |
| `deployment_state.at` | The time of the first `success` status of the deployment. |
| `deployment_state.restart` | `needed` is always false. GitHub has no restart. |

### Merge rule

`merge_merge_request` merges only when all of these hold.

1. The pull request is open.
2. The head commit has `pipeline_green`.
3. `mergeable_state` is `clean` or `has_hooks`. When GitHub has not computed it yet, the tool reads the pull request again after one second. When that second read shows a different head, the tool does not merge.
4. The argument `expected_head_sha` is required. It is the head commit that the caller tested. `head_sha` of `merge_request_state` and of `open_merge_request` gives it. When the head is another commit, the tool merges nothing and the reason says `the head ... moved: expected <sha>, now <sha>`. The tool sends the head sha that it checked to the host.

The tool sends the head sha that it checked in the merge call. GitHub refuses the merge with HTTP 409 when a push changed the head after the check. The result then has `merged: false` and the state `open`. The optional `merge_method` is `merge` or `squash`. Without it, the tool uses the first method that the repository allows: `merge`, then `squash`. It reads `allow_merge_commit` and `allow_squash_merge` of the repository before each merge. The tool does not offer a rebase merge, because `revert_merge_request` cannot revert one.

### Refused

A rule of the repository can block a merge: a missing review, a conflict, a draft, a branch that is behind. GitHub shows the rule in `mergeable_state`. GitHub also shows `blocked` while a required check has not reported. So the tool reports `refused` only when the checks are complete. While a required check is pending, the state is `open` and `pipeline_status` is `pending`. When the checks of the head passed and `mergeable_state` is not `clean`, `has_hooks`, `unstable` or `unknown`, both `merge_request_state` and `merge_merge_request` return the state `refused`. The reason is `GitHub does not allow the merge: <mergeable_state>`, for example `GitHub does not allow the merge: blocked`. `merge_merge_request` then does not call the merge.

The merge methods of the repository can block a merge too. Both tools return `refused` with one of these fixed reasons. When GitHub does not show `allow_merge_commit` or `allow_squash_merge`, the repository did not block the merge. The token cannot read the settings. GitHub says: "To view merge-related settings, you must have the contents:read and contents:write permissions." So both tools then return a tool error that names these two permissions, not `refused`.

| Cause | Reason |
|---|---|
| The repository allows neither a merge commit nor a squash merge. | `GitHub does not allow the merge: the repository allows neither a merge commit nor a squash merge, and the revert tool cannot undo a rebase merge` |
| The `merge_method` argument names a method that the repository does not allow. This applies to `merge_merge_request` only. | `GitHub does not allow the merge: the repository does not allow the merge method <method>` |

The server keeps no memory of a refusal. It reads the state from GitHub on each call. When a person removes the block, the next call returns `open`. Each replica of the server, and a server after a restart, gives the same answer.

A refusal of the merge call (HTTP 403, 405 or 422) returns `refused` in the result of that call. The reason is `GitHub refused the merge (HTTP <status>): <message of GitHub>`. The reason stays the same while the cause stays, so the daemon can see a repeat. GitHub does not show every cause of such a refusal in a read, so the next `merge_request_state` can return `open`.

A pull request from a fork, or from a fork that a person deleted, is never merged or reverted. The tool sees a fork when `head.repo.full_name` is not `base.repo.full_name` (the compare ignores case), or `head.repo` is null. A fork can hold a branch with any name, the name of a job branch too. `merge_merge_request` and `revert_merge_request` return `refused` with the fixed reason `the source branch is in a fork, not in the project of the merge request`. They change nothing on the host. A pull request that is already merged stays `merged` in the result of `merge_merge_request`.

### Revert

GitHub has no revert call in the REST API. `revert_merge_request` builds the revert with the git data API.

1. It reads the pull request. The pull request must be merged.
2. It looks for a pull request from the branch `zeroroot/revert-pr-<number>-<commit>`. It finds the work of an earlier call, so the same call twice opens one revert pull request. When a person closed that pull request without a merge, it tries the branch with the suffix `-2`, then `-3`. After three closed revert pull requests, the tool returns an error and a person reverts by hand. A closed revert never stops the loop without an answer.
3. It reads the base branch. The base branch must end at the merge commit.
4. When the revert branch exists already, the tool keeps it only when its last commit is the revert commit of the tool: one parent, which is the merge commit, and the tree of the first parent of the merge commit. Otherwise a person made the branch, and the tool refuses with an error.
5. It makes a commit with the tree of the first parent of the merge commit and the merge commit as its parent. It creates the branch at that commit and opens a pull request from the branch.
6. It merges the revert pull request by the merge rule above. When the checks have not finished, it returns the state of the revert pull request and `merged: false`. Call the tool again later to merge it.

Limits:

- The revert is exact only while no commit came after the merge. The tree of the parent of the merge commit holds no later work. When the base branch has later commits, the tool refuses and makes nothing. A person must revert by hand. This tool does not do a three-way merge, because the git data API has none.
- The tool reverts a merge commit and a squash commit. It refuses a rebase merge of more than one commit, because the merge commit of a rebase is the last of several commits. A revert of it would undo only part of the pull request. A merge commit with two parents is a merge commit. A merge commit with one parent comes from a squash merge or a rebase merge. After a squash merge, its parent is the tip of the base branch before the merge, which is never a commit of the pull request. After a rebase merge, its parent is a rebased commit of the pull request. A rebase gives each commit a new sha and keeps the author, the author date and the message. So the tool lists the commits of the pull request and refuses when the parent has the sha, or the author, the author date and the message, of one of them. GitHub lists at most 250 commits of a pull request, so the tool refuses a pull request with more commits. A second check does not use the author date. `GET /repos/{owner}/{repo}/commits/{parent}/pulls` lists "the merged pull request that introduced the commit to the repository". After a rebase merge, the pull request introduced the parent, so GitHub lists it. After a squash merge, another pull request or a push introduced the parent. The tool refuses when either check finds a rebase merge. It also refuses when the second call fails, or when it returns a full page of 100 pull requests without this one.
- A repository with no check run and no commit status has no `pipeline_green`. The tool opens the revert pull request and never merges it.
- The tool does not wait for the checks. The caller calls it again.
- The commit has the author of the token. GitHub shows it as unverified unless the token belongs to a GitHub App that signs commits.

## open_merge_request

The input follows the contract of the incident agent: `project` (`owner/repo`), `branch`, `base_branch`, `title`, `description` and `files`. A file is a `path` with `content` (the new text of the file) or `delete: true`. One call makes one commit with all changes, on the new source branch, from the target branch, and opens the pull request. The result holds `merge_request` (`owner/repo#12`), `url`, `source_branch`, `target_branch` and `created_at`.

The tool never writes to the target branch and never merges. The source branch is made from the head of the base branch. When the source branch exists, the tool compares it with the base branch (`GET /repos/{o}/{r}/compare/{base}...{head}`). It accepts the branch only when it holds exactly one commit beyond the base branch, and that commit changes exactly the files of the input with the git blob sha of the input content. It then returns the open pull request of the branch with `existing: true`, or opens one and makes no commit. The compare answer of GitHub has no file mode, so the tool also reads the tree of the commit (`GET /repos/{o}/{r}/git/trees/{sha}?recursive=1`) and requires the type `blob` and the mode that the tool writes for each written path (see below), and no entry for a removed path. A tree that GitHub cuts is refused. In every other case it refuses with the reason, and it never builds on a foreign commit. A branch whose commit changes nothing against the base branch does not match, so the tool refuses it. The undo is `close_merge_request`.

Each file is written with the mode `100644`, except a path that is an executable file (`100755`) in the tree of the base branch. That path keeps `100755`, so an edit of a script does not drop its executable bit. The tool reads the mode by walking the folders of each written path (`GET /repos/{o}/{r}/git/trees/{sha}` without `recursive`, https://docs.github.com/en/rest/git/trees#get-a-tree). A new path gets `100644`. There is no input field for the mode. The tool refuses, before any write, a write to a path that exists and is not a plain file (a symlink, a submodule or a folder), a write or a delete below a path that is not a folder, and a delete of a folder or a submodule, because that delete would remove it whole. It deletes files and symlinks only. A delete of a path that does not exist in the base branch is refused with a clear message, because there is nothing to delete. A resume reads the mode from the parent commit of the source branch. The input has these limits: 100 files, 1 MiB for one file, 8 MiB for all files, a title of 256 bytes, a branch name of plain characters, and a path with no `..`, no empty folder name and no `.git` folder.

References: https://docs.github.com/en/rest/git/blobs#create-a-blob, https://docs.github.com/en/rest/git/trees#create-a-tree, https://docs.github.com/en/rest/git/commits#create-a-commit, https://docs.github.com/en/rest/git/refs#create-a-reference and https://docs.github.com/en/rest/pulls/pulls#create-a-pull-request.

## read_file

The input is `project`, `ref` (a branch, a tag or a commit) and `path`. The result is `found`, `content`, `sha` (the blob sha), `size`, `path` and `ref`. `found` is always present. A file that does not exist on the ref gives `found: false` with no error. A 404 of GitHub means a missing path or a missing repository, so the tool reads the repository to tell them apart. A missing repository is an error. The tool refuses a path with `..`, a leading `/`, a backslash, an empty folder name or a `.git` folder. It refuses a folder, a file over 1 MiB and a file that is not UTF-8 text. It never writes.

Reference: https://docs.github.com/en/rest/repos/contents#get-repository-content.

## changed_files

`merge_request_state` also returns `changed_files`, a list of `{path, change, previous_path, blob_sha, mode}`, with `changed_files_truncated` and `changed_files_error`. `change` is `added`, `modified`, `deleted`, `renamed` or `mode_changed`. `mode` is the file mode at the head (for example `100644`). GitHub gives no file mode in this list, so `mode` is empty. The list has at most 100 files. The tool reads `GET /repos/{o}/{r}/compare/{base sha}...{head sha}`, with the base sha and the head sha of the pull request that the state returned. The files endpoint of the pull request reads the pull request as it is when the call runs. It can lag behind a push, and a head that goes from A to B and back to A passes a check of the head. A compare of two shas names the head, so the files cannot belong to another commit. GitHub gives the blob sha in `sha`, and the tool leaves `blob_sha` empty for a removed file. A status `copied` is `added`, `changed` is `mode_changed` (a change of the mode) and `unchanged` is `modified`. GitHub gives at most 300 files in a compare answer. The tool returns the first 100. `changed_files_truncated` is true when the answer has more than 100 files, or holds 300, which can be a cut list. The page of the API is https://docs.github.com/en/rest/commits/commits#compare-two-commits. When the host does not give the list, the state is still returned, `changed_files` is empty and `changed_files_error` holds the cause. An empty list with an error means that the files are unknown.

An agent must refuse each `mode_changed` entry. The GitHub status `changed` is a change of the mode, and it also covers a change of the type, for example a file that became a symbolic link. The list cannot tell the two apart, so a `mode_changed` entry is never a change that an agent can accept.

The files are those of the commits that the state names, so a push after the state read cannot change them.

## Deployment requirement

The MCP endpoint on `:8080` has no caller authentication. Any caller that reaches the port can call each tool with the vendor credential of this server. The ToolHive proxy of the connector authenticates the daemon in front of it.

1. Apply a NetworkPolicy that admits ingress to this port only from the ToolHive proxy pod of the connector.
2. Do not expose the port through a Service of type `LoadBalancer` or `NodePort`, an Ingress or a port-forward.

The server has no second authentication scheme, by design. The proxy and the NetworkPolicy are the access control.

## Vendor permission

Use a fine-grained token or a GitHub App installation, for the repositories that the agents fix and no others. It needs these repository permissions.

| Permission | Level | Reason |
|---|---|---|
| Metadata | Read | Required by GitHub. |
| Pull requests | Read and write | Read, open and merge a pull request. |
| Contents | Read and write | Merge a pull request. Create the revert commit and branch. Create the branch and the commit of `open_merge_request`. Compare two commits. Read `allow_merge_commit` and `allow_squash_merge`, which GitHub shows only with write access. |
| Checks | Read | Read the check runs. |
| Commit statuses | Read | Read the combined status. |
| Deployments | Read | Read the deployments and their statuses. |

Branch protection of the repository still applies to the token. A rule that needs a review blocks the merge, and the tool reports it as `refused`.

## Not verified

No call to a live GitHub. The tests run against an `httptest` mock that follows the API reference above. The mock does not prove these points:

- The text of a refusal on a real GitHub. The tool shows the message that GitHub sends.
- That `mergeable_state` is `clean` for every mergeable pull request. A repository with a merge queue or a ruleset can answer differently.
- That a tree of a merge commit parent gives the same tree as a Git revert on a real repository. It does when the base branch ends at the merge commit.
- That a rebase merge on a real GitHub keeps the author date of each commit. Git keeps it on a rebase. If GitHub changed it, the second check still sees the rebase merge.
- That `GET /commits/{parent}/pulls` lists the pull request of a rebase merge for its rebased commits. The API reference says so. No live call has confirmed it.
- That `GET /branches/{base}` shows `protection.required_status_checks` to a token without admin access. When it shows no required check, the tool falls back to `mergeable_state`.

## list_dependency_alerts

The tool lists the Dependabot alerts of a repository. The gibson scan mission reads the alerts of the buyer with it. A rescan after a fix reads them again. A merged fix makes the alert `fixed`.

Endpoint: `GET /repos/{owner}/{repo}/dependabot/alerts`. Reference: https://docs.github.com/en/rest/dependabot/alerts#list-dependabot-alerts-for-a-repository

Token permission: the fine-grained permission "Dependabot alerts" with read access on the repository. A classic token needs the `security_events` scope (or `repo`). The tool also reads the repository and its manifest files, so the token needs the "Contents" read permission.

Input: `project` (`owner/repo`), `state` (`open` by default, `fixed`, `dismissed` or `all`) and `ref` (optional). GitHub reports the alerts of the default branch only. The tool refuses a `ref` that is not the default branch.

Result: `project`, `ref`, `state`, `alerts` and `truncated`. Each alert has these fields.

| Field | Source |
|---|---|
| `id` | `dependabot:<number>`. The number is stable. |
| `state` | `open`, `fixed` or `dismissed`. `auto_dismissed` maps to `dismissed`. |
| `package`, `ecosystem` | `dependency.package.name` and `.ecosystem` (`go`, `npm`, `pip`, `maven`, `rubygems` and so on). |
| `purl_type` | The ecosystem as the type of a package URL: `golang`, `npm`, `pypi`, `maven`, `gem`, `cargo`, `composer`, `nuget`, `pub`, `swift`, `hex`, `githubactions`. |
| `version`, `version_source` | The installed version at the default branch. GitHub gives none, so the tool reads it from the manifest of the alert. `version_source` is `manifest`. See the next section. |
| `versions`, `version_ambiguous` | Set when the manifest installs the package at more than one version. `versions` lists them. `version`, `version_source` and `purl` are then empty, so a caller does not match the alert to the wrong package. |
| `purl` | The package URL with the version, for example `pkg:npm/lodash@4.17.20` or `pkg:golang/github.com/acme/lib@v1.2.0`. The trivy parser of the executor uses the package URL as the identity of a package, so the same package has the same `purl`. Empty when the version is not known. |
| `vulnerable_version_range`, `first_patched_version` | `security_vulnerability.vulnerable_version_range` and `.first_patched_version.identifier`. |
| `advisory_ids`, `verifiable` | The GHSA id and then the CVE id, from `security_advisory`. `verifiable` is false when the list is empty. |
| `severity` | `low`, `medium`, `high` or `critical`. |
| `manifest_path`, `url` | `dependency.manifest_path` and `html_url`. |

The installed version comes from the manifest at the default branch (`GET /repos/{o}/{r}/contents/{manifest_path}?ref=<default branch>`). The tool knows `go.mod`, `package-lock.json` and `requirements.txt`. For `go.mod` it reads the `require` lines and applies the `replace` directives as trivy does: a replace to a module gives the name and the version of the new module, and a replace to a local path gives no version. For `package-lock.json` it reads each `node_modules/<name>` entry, nested copies too (lockfile 2 and 3), and each `dependencies` entry, nested ones too (lockfile 1). For `requirements.txt` it reads `name==version` and `name===version`. A range or a wildcard such as `1.2.*` is not a version. For another manifest, `version`, `version_source` and `purl` are empty. The tool reads at most 20 manifests in one call.

Pages: the tool reads 100 alerts a page by cursor (the `after` value of the `Link` header), at most 10 pages. When GitHub has more alerts, `truncated` is true. A next page link without an `after` cursor is an error, not the end of the list. A GitHub error such as 403 (alerts disabled, or a token without the permission) is an error, never an empty list.

How to verify a fix: call the tool with `state` `all`. Treat an alert as fixed only when its `state` is `fixed`. An alert that is missing, a `dismissed` alert (this includes `auto_dismissed`) and a `truncated` list prove nothing. The package URL is built with `github.com/package-url/packageurl-go`. The tool folds the name as trivy does: a Go module path, an npm name and a Python name are lower case.

Test mock: `mock-forge` serves the endpoint from the content of the repository. See the README of `mock-forge`.

## list_tree

The tool lists the entries of the repository tree, so a reader needs no git clone. Input: `project`, `ref` (a branch, a tag or a commit, the default branch when empty), `path` (a folder, optional) and `cursor` (optional). The tool lists every level below `path`. Each entry has `path` (the full path), `type` (`file`, `dir`, `symlink` or `submodule`), `sha` (the git object sha), `size` (bytes, files and symlinks), and `mode` (`100644`, `100755`, `120000`, `040000` or `160000`). The result has `next_cursor` (empty on the last page), `truncated` and `reason`. When `truncated` is true, the list is not complete, and the caller lists a smaller folder with `path`. The types and the limits are in `internal/fixloop/tree.go`.

Endpoint: `GET /repos/{owner}/{repo}/git/trees/{tree_sha}`. Reference: https://docs.github.com/en/rest/git/trees#get-a-tree . `tree_sha` is a sha or a ref name. With `recursive`, GitHub returns the folders and the files below the tree, at most 100,000 entries or 7 MB, and sets `truncated` when the tree is larger. The tool walks the folders of `path` without `recursive`, then reads the subtree with `recursive`. A page has at most 1000 entries. The cursor is `<tree sha>:<offset>:<hash>`. It names the tree by its sha, so the pages of one listing come from one tree even when the branch moves. The hash binds the cursor to the project and the path, and the tool refuses a cursor of another project or another path. GitHub answers at most 8 MiB to the tool (the limit of the client). A larger tree gives an error that tells the caller to list a smaller folder with `path`. A cut tree gives `truncated` true on each page. The token needs the Contents read permission.
