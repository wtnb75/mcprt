# mcprt

mcprt aggregates multiple MCP servers (local stdio subprocesses and remote
HTTP servers) behind a single MCP gateway endpoint, relaying `tools/*` and
`resources/*` calls to whichever backend serves them.

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

      - name: remote-filesystem
        transport: stdio
        command: ["mcp-server-filesystem", "--root", "/data"]
        dir: "/data"        # working directory on the remote host
        env:
          FOO: bar          # exported on the remote host before command runs
        ssh:
          host: "user@example.com"
          port: 2222                              # optional
          identity_file: "/home/me/.ssh/id_ed25519" # optional, passed as -i
          args: ["-o", "StrictHostKeyChecking=no"]  # optional, appended to the ssh invocation

      - name: github
        transport: http
        url: "http://localhost:9090/mcp"
        headers:
          Authorization: "Bearer ${GITHUB_TOKEN}"
        prefix: "gh__"
        proxy: "http://user:${PROXY_TOKEN}@proxy.example.com:8080" # optional; fixed proxy for this backend only

      - name: internal-api
        transport: http
        url: "http://internal.example.com/mcp"
        proxy: "none" # bypass HTTP_PROXY/HTTPS_PROXY for this backend, even if set

    overrides:
      gh__search: github

    resource_overrides:
      "file:///data/README.md": filesystem

    resource_template_overrides:
      "file:///data/{path}": filesystem

`overrides` resolves conflicting **tool** names (after each backend's
`prefix` is applied). `resource_overrides` and `resource_template_overrides`
resolve conflicting resource URIs and URI templates the same way, but
`prefix` is never applied to resources: a URI already carries a
backend-specific namespace (`scheme://host/path`), and string-concatenating
a prefix onto one would produce an invalid URI. `resources/subscribe` and
`notifications/resources/updated` are not relayed.

When `ssh` is set on a stdio backend, mcprt runs `command` on the remote host
by shelling out to the local `ssh` binary (so `~/.ssh/config`, `ssh-agent`,
and `known_hosts` all apply as usual); `dir` and `env`/`env_file` are applied
on the remote side, not the local one.

For an http backend, `proxy` controls outbound proxying per backend: unset
follows the usual `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` environment
variables, a URL forces that fixed proxy, and `"none"` forces a direct
connection regardless of those environment variables.

v1 has no built-in gateway authentication, so keep `listen.http` bound to
localhost or a trusted network and put a reverse proxy (or equivalent) in
front of it before exposing it any further.

## Development

    task build   # build ./bin/mcprt
    task test    # go test ./...
    task lint    # gofmt -l . && go vet ./... && golangci-lint run ./...
    task fmt     # gofmt -w .
