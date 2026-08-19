# mcprt

mcprt aggregates multiple MCP servers (local stdio subprocesses and remote
HTTP servers) behind a single MCP gateway endpoint.

## Usage

    mcprt server --config config.yaml [--log-level info]

## Configuration

See `docs/superpowers/specs/2026-08-19-mcprt-gateway-design.md` for the full
design, including the config file format and conflict-resolution rules.

Minimal example:

    listen:
      stdio: true
      http: ":8080"

    backends:
      - name: filesystem
        transport: stdio
        command: ["mcp-server-filesystem", "--root", "/data"]

      - name: github
        transport: http
        url: "http://localhost:9090/mcp"
        headers:
          Authorization: "Bearer ${GITHUB_TOKEN}"
        prefix: "gh__"

    overrides:
      gh__search: github

## Development

    task build   # build ./bin/mcprt
    task test    # go test ./...
    task lint    # gofmt -l . && go vet ./... && golangci-lint run ./...
    task fmt     # gofmt -w .
