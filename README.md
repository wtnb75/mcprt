# mcprt

mcprt aggregates multiple MCP servers (local stdio subprocesses and remote
HTTP servers) behind a single MCP gateway endpoint, relaying `tools/*`,
`resources/*`, and `prompts/*` calls to whichever backend serves them.

## Usage

    mcprt server --config config.yaml [--log-level info] [--log-format text]

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

      - name: containerized
        transport: stdio
        command: ["mcp-server-filesystem", "--root", "/data"]
        docker:
          bin: podman            # docker-compatible CLI to invoke; defaults to "docker" ("podman", "nerdctl", ...)
          image: "your/mcp-image"
          args: ["-v", "/data:/data"]  # extra arguments appended to "run"
          env:
            DOCKER_HOST: "ssh://user@example.com" # env for the CLI process itself, not the container

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

    prompt_overrides:
      code-review: filesystem

    logging:
      mask_keys: ["internal_id"] # extra key-name substrings to mask in the audit log, in addition to the built-in key/auth/pass/cred/token patterns

    timeouts:
      shutdown: 5s              # default 5s; graceful HTTP shutdown's upper bound
      telemetry_shutdown: 5s    # default 5s
      backend_connect: 30s      # default 30s; per-backend connect attempt, and connectBackends' startup collection window
      reload_drain: 5m          # default 5m; how long a SIGHUP-superseded generation's backends stay open
      elicit: 5m                # default 5m; how long a relayed elicitation request waits for a client answer
      progress_relay: 5s        # default 5s; relaying one progress notification to its requesting client
      backend_backoff_min: 1s   # default 1s; reconnect backoff floor
      backend_backoff_max: 60s  # default 60s; reconnect backoff ceiling
      backend_keepalive: 30s              # default 0 (disabled); interval for a periodic MCP ping to every backend
      backend_keepalive_failure_threshold: 3  # default 0 (defers to 1); consecutive ping failures tolerated before closing the connection
      downstream_keepalive: 30s              # default 0 (disabled); interval for a periodic MCP ping to every downstream client
      downstream_keepalive_failure_threshold: 3  # default 0 (defers to 1); consecutive ping failures tolerated before closing that client's session

`overrides` resolves conflicting **tool** names (after each backend's
`prefix` is applied). `resource_overrides` and `resource_template_overrides`
resolve conflicting resource URIs and URI templates the same way, but
`prefix` is never applied to resources: a URI already carries a
backend-specific namespace (`scheme://host/path`), and string-concatenating
a prefix onto one would produce an invalid URI. `resources/subscribe` and
`notifications/resources/updated` are not relayed.
`prompt_overrides` resolves conflicting **prompt** names, the same way
`overrides` resolves tool names — including `prefix` being applied to
prompt names before conflict resolution, exactly like tool names (unlike
resource/resource-template URIs, which never get a prefix).
`notifications/prompts/list_changed` and `completion/complete` are not
relayed.

Every backend call (`tools/call`, `resources/read`, `prompts/get`) is logged
one line per call, success or failure, at `info`/`error` level respectively,
with the calling MCP client's name/version, session ID, HTTP remote address
(HTTP sessions only), call duration, and the call's arguments. Any argument
object key matching (case-insensitively, by substring) `key`, `auth`,
`pass`, `cred`, `token`, or an entry in `logging.mask_keys` has its value
replaced with `***` before logging.

`timeouts` overrides mcprt's built-in timeout/backoff defaults; every field
is optional and falls back to the default noted above when unset. Values are
Go duration strings (`"5s"`, `"1m30s"`, ...). `timeouts` is read once at
process startup, including on `mcprt ping`/`call`/`list` one-shot commands —
a config reload via SIGHUP does not pick up changes to it, so changing a
timeout requires restarting `mcprt server`.

`backend_keepalive` is the one field in `timeouts` that's opt-in rather than
a tunable default: it's 0 (disabled) unless set. When enabled, every backend
connection sends an MCP `ping` on that interval and closes itself after
`backend_keepalive_failure_threshold` consecutive failures — which mcprt's
existing reconnect logic then picks up exactly like any other disconnect.
This detects a backend that's gone silent (hung, or vanished without
closing the connection) without waiting for an actual `tools/call` to time
out against it. Enabling it means a ping request per backend per interval,
so leave it disabled (the default) if you have many backends or an
unreliable network to them.

`downstream_keepalive`/`downstream_keepalive_failure_threshold` are
`backend_keepalive`'s counterpart for the other direction: mcprt pings each
*downstream* client instead, closing its session after that many
consecutive failures once it's gone silent (crashed, network drop),
freeing that session's resources without waiting for its next request.
Also opt-in (disabled by default) for the same reason. Unlike every other
`timeouts.*` field, this one IS re-read on every SIGHUP config reload: it's
applied per hot-reload generation rather than once at process startup (see
`gateway.NewConfig`), so a new value takes effect for new downstream
connections without a restart -- sessions from before the reload keep
whatever setting was in effect when they connected.

mcprt also participates in distributed tracing via OpenTelemetry: a call
served over the HTTP transport gets wrapped in a span (continuing an
inbound `traceparent` header if present), and if the routed backend is
itself an HTTP backend, the active span's trace context is injected into
the outbound request, so the trace continues across the hop. Only trace
context is forwarded to backends, never OpenTelemetry baggage, since
baggage would otherwise carry client-controlled data past the audit log's
redaction. Tracing is configured entirely through standard `OTEL_*`
environment variables (`OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_TRACES_EXPORTER`,
`OTEL_SERVICE_NAME`, ...) — there is no mcprt-specific config for it. Set
`OTEL_TRACES_EXPORTER=none` to disable tracing entirely; the unconfigured
default target is `https://localhost:4318`, so a connection-refused error on
an https URL is expected when nothing is configured, and should not be
treated as a startup failure. Calls made over the stdio transport are never
traced (there is no out-of-band channel to carry trace context on stdio).
When a call is traced, its audit log line (see above) also carries
`trace_id`/`span_id` fields. Resource-template reads use the distinct span
name `resources/templates/read` rather than `resources/read`, even though
both serve the same MCP method (`resources/read`), so a trace search for
`resources/read` alone will miss template reads.

When `ssh` is set on a stdio backend, mcprt runs `command` on the remote host
by shelling out to the local `ssh` binary (so `~/.ssh/config`, `ssh-agent`,
and `known_hosts` all apply as usual); `dir` and `env`/`env_file` are applied
on the remote side, not the local one.

When `docker` is set on a stdio backend, mcprt runs `command` inside a
container by shelling out to `docker.bin` (default `"docker"`; set it to
`"podman"`, `"nerdctl"`, etc. for another docker-compatible CLI) as
`<bin> run -i --rm ... <image> <command>`. `dir` and `env`/`env_file` are
applied inside the container (as `-w` and `-e`), same as the plain local
case; `docker.env` is separate and applies to the local CLI process itself
(e.g. `DOCKER_HOST` to point it at a remote daemon), not the container.
`docker.args` is appended to `run` verbatim for anything else (volumes,
networks, resource limits, ...). `ssh` and `docker` are mutually exclusive.

For an http backend, `proxy` controls outbound proxying per backend: unset
follows the usual `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` environment
variables, a URL forces that fixed proxy, and `"none"` forces a direct
connection regardless of those environment variables.

v1 has no built-in gateway authentication, so keep `listen.http` bound to
localhost or a trusted network and put a reverse proxy (or equivalent) in
front of it before exposing it any further.

## Container health checks

`mcprt server` with `listen.http` set answers `GET /healthz` with a bare
`200 OK` — no MCP session, no backend I/O. It only proves the process is up
and the HTTP listener is dispatching requests, so it's safe to wire to a
**liveness** probe: a backend going down is normal, auto-recovering
behavior for mcprt (see `superviseBackend`'s reconnect loop and
`timeouts.backend_keepalive` above), and a liveness probe that depends on
backend health would fight that design by restarting the whole process over
a problem mcprt is already handling on its own.

docker compose:

    services:
      mcprt:
        # ...
        healthcheck:
          test: ["CMD", "wget", "-q", "-O-", "http://localhost:8080/healthz"]
          interval: 10s
          timeout: 3s
          retries: 3

Kubernetes:

    livenessProbe:
      httpGet:
        path: /healthz
        port: 8080
      periodSeconds: 10

If you also want a **readiness** check that reflects backend health (e.g.
to stop routing traffic to a pod whose backends aren't up yet), use
`mcprt ping --config config.yaml` as an exec probe instead — it exits 0 only
if every configured backend connects and lists tools successfully, and
non-zero otherwise:

    readinessProbe:
      exec:
        command: ["mcprt", "ping", "--config", "/etc/mcprt/config.yaml"]
      periodSeconds: 10

Don't use `mcprt ping` for **liveness**: unlike `/healthz`, it depends on
every backend being reachable, so a k8s liveness probe wired to it would
restart the mcprt process itself over a transient backend outage instead of
just marking the pod not-ready — the readiness/liveness split above is
deliberate, not interchangeable.

`listen.stdio`-only deployments (no `listen.http`) have no `/healthz` to
probe, since there's no HTTP listener at all; use `mcprt ping` (exec) for
health checks in that case.

## Development

    task build   # build ./bin/mcprt
    task test    # go test ./...
    task lint    # gofmt -l . && go vet ./... && golangci-lint run ./...
    task fmt     # gofmt -w .
