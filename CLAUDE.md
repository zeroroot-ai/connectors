# CLAUDE.md

Read [AGENTS.md](./AGENTS.md). It holds the rules of this repo.

| Command | Action |
|---|---|
| `make test` | Run the unit tests. |
| `make check` | Run gofmt, `go vet` and the server guard. |
| `make new-server NAME=<tool>` | Start a server from `_template/`. |

The repo has no CodeGraph index. Use `git grep`.
