# connectors

This repo holds the MCP servers that Zero Root writes for security tools whose vendor publishes none (ADR-0177). Each server runs behind a Gibson connector. The Gibson catalog pins the image of each server by digest.

## Layout

One directory for each tool. A directory holds one Go module, one `Dockerfile` and the tests of that server.

## Rules

- A server holds no credential in its image. It reads each credential from its environment.
- Each tool of a server has a test against a mock of the vendor API.
- A tool that changes its target states if a person can undo the change, and it names the tool that does the undo (ADR-0180).
- A server asks for the least API permission that its tools need. Its README states that permission.

## License

Elastic License 2.0. See `LICENSE`.
