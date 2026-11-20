# AGENTS.md

Rules for an agent or a person who changes this repo. The workflow contract is `zeroroot-ai/.github` `AGENTS.md`. If this file and that file disagree, that file wins.

## What this repo is

The MCP servers of ADR-0177 and ADR-0180. Gibson holds the manifest of each connector. This repo holds the code and the image of the server.

## Rules

1. One server is one directory. Start it with `make new-server NAME=<tool>`.
2. Put shared code in `internal/kit`. Run `git grep` before you add a helper. Do not write a second one.
3. Do not read a credential from anything but the environment. Do not log one. Wrap each credential in `kit.Secret`.
4. Do not build a URL from an input. Pass path segments to `kit.Path`. Check each input with `kit.CheckString`.
5. Do not run a vendor call from a test. Each test uses an `httptest` mock that follows the public API reference of the vendor. Cite the page in a comment.
6. If you cannot find the real request and response shape of a vendor call, do not write the tool. Report the gap in the issue.
7. Do not add a `schedule` trigger to a workflow. Do not add `replace` directives or a `go.work`.
8. Do not tag or bump a version by hand. release-please does it.
9. Write prose in Simplified Technical English.

## Heavy commands

Run `go test`, `go build`, `go vet` and `make check` through `scripts/heavy` of the workspace. Run `golangci-lint` only with the memory cap that the workspace `CLAUDE.md` states.
