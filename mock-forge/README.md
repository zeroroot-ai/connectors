# mock-forge

TEST IMAGE. Do not run it in a production install.

`mock-forge` is a stateful mock of the GitHub REST API and of the Prometheus HTTP API. It is no MCP server. It stands in for the forge and the metrics of a buyer in the exit test of the fix loop (gibson#1469). The `github-fixloop` and `prometheus` servers talk to it with no change. The tests of both servers use the same code (`internal/mockforge`).

The mock holds all state in memory. It checks no credential and it sends no data out. The image has the label `ai.zeroroot.image-class=test`. It is `ghcr.io/zeroroot-ai/connectors-mock-forge`.

## Environment

| Variable | Meaning |
|---|---|
| `LISTEN_ADDR` | Optional. The default is `:8080`. |
| `MOCK_ALLOW_PLAINTEXT` | Optional. Set it to `1` to serve plain HTTP on an address that is not loopback. |
| `MOCK_TLS_CERT_FILE`, `MOCK_TLS_KEY_FILE` | Optional. Serve https with this certificate. |
| `MOCK_TLS_SELF_SIGNED_HOSTS` | Optional. A comma list of host names or IP addresses. The mock makes an authority and a certificate for them, and serves https. |
| `MOCK_TLS_CA_FILE` | Optional. With `MOCK_TLS_SELF_SIGNED_HOSTS`, the mock writes the PEM of its authority to this path. `GET /_mock/ca.pem` also serves it. |

The mock has no credential, so anyone who reaches the port can reset or seed it. It refuses to start with plain HTTP on an address that is not loopback. Use TLS, or set `MOCK_ALLOW_PLAINTEXT=1`, or listen on `127.0.0.1`. The default `:8080` is not loopback.

A client that must trust the self-signed certificate loads the PEM of the authority, for example with `SSL_CERT_FILE`.

## GitHub endpoints

The endpoints are exactly the calls of `github-fixloop`. The API version is 2022-11-28. A GitHub Enterprise path prefix `/api/v3` also works.

| Call | Reference |
|---|---|
| `GET /repos/{o}/{r}` | https://docs.github.com/en/rest/repos/repos#get-a-repository |
| `GET`, `POST /repos/{o}/{r}/pulls`, `GET`, `PATCH /pulls/{n}`, `PUT /pulls/{n}/merge`, `GET /pulls/{n}/commits` | https://docs.github.com/en/rest/pulls/pulls |
| `GET /repos/{o}/{r}/commits/{sha}/check-runs` | https://docs.github.com/en/rest/checks/runs#list-check-runs-for-a-git-reference |
| `GET /repos/{o}/{r}/commits/{sha}/status` | https://docs.github.com/en/rest/commits/statuses#get-the-combined-status-for-a-specific-reference |
| `GET /repos/{o}/{r}/commits/{sha}/pulls` | https://docs.github.com/en/rest/commits/commits#list-pull-requests-associated-with-a-commit |
| `GET /repos/{o}/{r}/compare/{base}...{head}` | https://docs.github.com/en/rest/commits/commits#compare-two-commits |
| `GET /repos/{o}/{r}/branches/{branch}`, `GET /rules/branches/{branch}` | https://docs.github.com/en/rest/branches/branches#get-a-branch |
| `GET /git/ref/heads/{branch}`, `POST /git/refs` | https://docs.github.com/en/rest/git/refs |
| `GET`, `POST /git/commits` | https://docs.github.com/en/rest/git/commits |
| `POST /git/blobs`, `POST /git/trees`, `GET /git/trees/{sha}` | https://docs.github.com/en/rest/git/blobs and https://docs.github.com/en/rest/git/trees |
| `GET /repos/{o}/{r}/deployments`, `GET /deployments/{id}/statuses` | https://docs.github.com/en/rest/deployments/deployments |
| `GET /repos/{o}/{r}/git/trees/{tree_sha}?recursive=1` | https://docs.github.com/en/rest/git/trees#get-a-tree |
| `GET /repos/{o}/{r}/dependabot/alerts` | https://docs.github.com/en/rest/dependabot/alerts#list-dependabot-alerts-for-a-repository |
| `GET /repos/{o}/{r}/contents/{path}` | https://docs.github.com/en/rest/repos/contents#get-repository-content |

Rules of the mock:

- A merge with `sha` that is not the head answers 409. A closed or conflicting pull request answers 405.
- The mock supports the merge methods `merge` and `squash`. A merge makes a real merge commit with a three-way merge of the files.
- A path conflict makes `mergeable_state` `dirty` and `mergeable` false.
- A new commit gets the default check runs. The default is one check `ci` that passed. A test can change the default or the checks of one commit.
- The compare answer lists `added`, `modified`, `removed` and `changed` (a mode change). It does not detect a rename. As GitHub does, it gives no file mode.
- A tree has only blob entries. A path with a slash is one entry.

## Dependabot alerts

The mock derives the alerts from the content of the repository, so no test action closes an alert. A test seeds an advisory with `POST /_mock/advisories`:

```json
{"ghsa_id": "GHSA-c2f3-g4h5-j6m7", "cve_id": "CVE-2026-1111", "package": "github.com/acme/lib", "ecosystem": "go",
 "vulnerable_version_range": ">= 1.0.0, < 1.2.4", "first_patched_version": "1.2.4", "severity": "high"}
```

The fixture ecosystem is `go`. The mock reads `go.mod` on the default branch and applies its `replace` directives as `github-fixloop` does (a replace to a module gives that module and version, a replace to a local path gives no version). The alert is `open` while `go.mod` installs a version that the range holds. The alert is `fixed` after a commit to the default branch (a push, or a merged pull request) moves the requirement to a version outside the range, or removes it. If a later commit makes the version vulnerable again, the alert opens again. `POST /_mock/repos/{o}/{r}/alerts/{n}/dismiss` dismisses an open alert. The body `{"auto": true}` makes the state `auto_dismissed`. `PATCH /_mock/repos/{o}/{r}` with `{"dependabot_alerts_disabled": true}` makes the endpoint answer 403. A missing repository answers 404. A `state` value that is not `auto_dismissed`, `dismissed`, `fixed` or `open` answers 422. The range is comparisons that a comma joins: `>=`, `<=`, `<`, `>` and `=`. As GitHub does, the alert gives no installed version. The advisory applies to each repository, and the alert appears when the repository requires the package at a vulnerable version. The endpoint supports `state` (a comma list), `per_page` (at most 100) and the cursor `after` of the `Link` header.

## Git trees

`GET /repos/{o}/{r}/git/trees/{tree_sha}` takes a tree sha, a commit sha or a branch name. The mock keeps a tree as a flat map of paths, so it makes the entries of the folders and their tree shas when it lists. A recursive answer has the folders and the files. A blob entry has a size. A file with the mode `120000` is a symlink and a file with the mode `160000` is a submodule (type `commit`). `PATCH /_mock/repos/{o}/{r}` with `{"tree_limit": 5}` cuts a recursive answer after 5 entries and sets `truncated` to true, as GitHub does at 100,000 entries.

## Prometheus endpoints

The endpoints follow https://prometheus.io/docs/prometheus/latest/querying/api/ : `POST` and `GET /api/v1/query`, `/api/v1/query_range` and `GET /api/v1/alerts`.

The mock does not parse PromQL. A health value is a number that a test sets. It holds from the time of the call until the next call. A query with no value of its own gets the default value. With no value at all, the answer is 1, which is healthy.

## Control API

A test seeds and reads the state under `/_mock/`. No real API uses this prefix.

| Call | Body | Effect |
|---|---|---|
| `POST /_mock/repos` | `owner`, `repo`, `default_branch` (default `main`), `files`, and the settings below | Make a repository with one commit. The answer has `head_sha` and the blob sha of each file. |
| `GET /_mock/repos/{o}/{r}` | | Read the files of each branch, the pull requests, the deployments and the settings. |
| `PATCH /_mock/repos/{o}/{r}` | the settings | Change the settings. |
| `POST /_mock/repos/{o}/{r}/commits` | `branch`, `message`, `files` | Push a commit. A new branch starts at the default branch. |
| `POST /_mock/repos/{o}/{r}/checks` | `sha` or `branch` (none means the default), `checks` | Set the check runs. |
| `PUT /_mock/repos/{o}/{r}/merge-failure` | `status`, `message` | Make each merge call answer this error. `DELETE` clears it. |
| `POST /_mock/repos/{o}/{r}/deployments` | `sha` or `branch`, `environment`, `state` | Record a deployment. |
| `POST /_mock/prometheus/health` | `value`, optional `query`, `at` (RFC 3339), `no_data` | Set the health value from `at` (default now). |
| `POST /_mock/prometheus/alerts` | `alerts` | Set the alerts. |
| `GET /_mock/state`, `POST /_mock/reset` | | Read the summary, or drop all state. |

A file in `files` is a string (a regular file, mode 100644), or an object `{"content": "...", "mode": "100755"}`. A null value deletes the path.

The settings are `allow_merge_commit`, `allow_squash_merge`, `required_checks` (a list of names), `checks` (the default check runs, each with `name`, `status`, `conclusion`) and `auto_deploy`. With `auto_deploy` `{"environment": "production", "state": "success"}`, each commit that lands on the default branch gets a deployment. This stands in for the delivery pipeline of the buyer.

### Example

Seed a repository with one flawed file and an automatic deploy:

```bash
curl -s -X POST "$MOCK/_mock/repos" -d '{
  "owner": "acme", "repo": "shop",
  "files": {"src/db.go": "package db\n// flaw\n"},
  "auto_deploy": {"environment": "production", "state": "success"}
}'
```

Make the service broken after the deploy:

```bash
curl -s -X POST "$MOCK/_mock/prometheus/health" -d '{"value": 0}'
```

Read the file on `main`:

```bash
curl -s "$MOCK/_mock/repos/acme/shop" | jq '.branches.main.files'
```

## Connect the connectors

`github-fixloop` reads `GITHUB_API_URL`. `prometheus` reads `PROMETHEUS_BASE_URL`. Set both to the URL of the mock. The Gibson daemon accepts a value only when an install pattern allows it, and only for an https URL with a public host name. The mock needs the TLS variables above in that case.

## Build and test

```bash
go test ./internal/mockforge/... ./github-fixloop ./prometheus
```

The tests of `github-fixloop` (`forge_test.go`) open, read, merge and revert a pull request against the mock. The tests of `prometheus` (`forge_test.go`) set the health value and read it with `health_signal`.
