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
      http: "127.0.0.1:8080"

    backends:
      - name: filesystem
        transport: stdio
        command: ["mcp-server-filesystem", "--root", "/data"]
        dir: "/data"
        env_file: ".env"   # .env-format file, merged into env (env: takes precedence on conflicts)

      - name: github
        transport: http
        url: "http://localhost:9090/mcp"
        headers:
          Authorization: "Bearer ${GITHUB_TOKEN}"
        prefix: "gh__"

    overrides:
      gh__search: github

v1 has no built-in gateway authentication, so keep `listen.http` bound to
localhost or a trusted network and put a reverse proxy (or equivalent) in
front of it before exposing it any further.

## Development

    task build   # build ./bin/mcprt
    task test    # go test ./...
    task lint    # gofmt -l . && go vet ./... && golangci-lint run ./...
    task fmt     # gofmt -w .
