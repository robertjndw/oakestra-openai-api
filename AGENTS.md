# AGENTS.md

Oakestra OpenAI API addon: define LLM models, place them on nodes by hand, serve them behind one
OpenAI-compatible endpoint. Go 1.26 module `github.com/robertjndw/oakestra-openai-api`,
`CGO_ENABLED=0`, env prefix `OOA_`, addon name `openai-api`, Tailwind prefix `oai`.
README.md is the user documentation; keep its configuration table in sync with
`internal/config/config.go`.

## Commands

```sh
make check      # tidy-check, fmt-check, lint, test-race: must pass before you are done
make fmt        # gofmt + goimports
make test       # go test -shuffle=on ./...
make build      # every cmd/* into bin/
make vuln       # govulncheck
make up / down  # compose demo stack (needs ../dashboard-v2 running via compose.dev.yml)
make smoke      # deploy/smoke.sh against the running demo stack
```

Frontend, in `frontend/addon` (needs the `../dashboard-v2` checkout at the commit in
`DASHBOARD_REF`, with `pnpm install` run there):

```sh
pnpm install && pnpm typecheck && pnpm test && pnpm build
```

Tools (golangci-lint, govulncheck) are pinned in `tools/go.mod` and run via
`go tool -modfile=tools/go.mod`; never add them to the root `go.mod`.

Demo: in `../dashboard-v2` run `docker compose -f compose.dev.yml up -d --build`, then here
`docker compose up -d --build`. Dashboard at http://localhost:8080 (Admin / Admin), public API at
:8095. The fake Oakestra has simulated nodes and engines that echo canned text.

## Layout and ownership

```
cmd/ooa-server        wiring only
cmd/ooa-fakeoak       fake Oakestra with simulated nodes and fake engines
internal/config       env parsing and validation
internal/store        SQLite, migrations, settings (keys listed in settings.go)
internal/httpx        JSON helpers, error envelope, middleware, Serve
internal/oakestra     Oakestra client, SLA builder, oakestratest fake
internal/deploy       owns tables models and deployments; lifecycle loop, port allocation
internal/scheduler    Strategy, registry, least-inflight; stdlib only
internal/gateway      /v1 proxy; endpoint table fed by SetEndpoints
internal/auth         owns table api_keys; dashboard token introspection
internal/api          admin REST handlers (/admin/v1)
frontend/addon        UI addon
```

Import direction: `cmd -> api -> {deploy, gateway, auth, scheduler} -> {oakestra, store, httpx,
config}`. Rules that follow from it:

- Only `internal/oakestra` builds requests to Oakestra.
- `gateway` never imports `deploy`; `cmd` wires `deploy.Manager.OnChange` to
  `gateway.SetEndpoints` and `gateway.OnUpstreamFailure` to the loop's kick.
- `scheduler` imports only the standard library.
- A table is written only by its owner package; the `settings` table goes through `store`.

## Conventions

- Every package has a `doc.go`; update it when behaviour changes.
- Errors for HTTP go through the `httpx` helpers and the error envelope. The `/v1` gateway renders
  OpenAI-shaped errors itself (`internal/gateway/errors.go`).
- Timestamps are unix milliseconds (`store.Millis`, `store.FromMillis`).
- Tests use the standard library only and run against `oakestratest`, not a real Oakestra.
- No em dashes or en dashes anywhere (code, docs, commits); use plain hyphens or rephrase.
- Comments explain why, not what, and never announce that something was changed.
- Conventional Commits: `type: subject`, lowercase, no trailing period (`feat`, `fix`, `refactor`,
  `docs`, `chore`, `test`, `build`).
- Never stage or commit unless asked.
- If `CLAUDE.md` is a symlink to this file, edit `AGENTS.md`.

## Oakestra wire traps

Recorded in `internal/oakestra/doc.go`; read it before touching the client. In short: login and
refresh shapes, `POST /api/application/` returns all applications and 409 means it exists, service
creation answers 200 even when a service failed (`failed_services`), nodes come from each cluster's
resource abstractor and not the system manager, deleting a missing service answers 500, direct
constraints match cluster and node names, SLA `cmd` replaces ENTRYPOINT and CMD, a volume's
`config` reaches the CSI driver as VolumeContext, and the scheduler only places a service on a node
that advertises every CSI driver its volumes name.

## Invariants

- Nothing is deleted because a node vanished from a listing. Only the user removes deployments.
- The Oakestra service id is persisted before an instance is deployed, so a crash in between
  never leaks a service.
- `volume_id` is unique per deployment and never starts with `pvc-`.
- Nothing from `/admin` is served on the public listener, and the two listeners must differ.
- `/v1` responses carry no node or deployment details unless `OOA_DEBUG_HEADERS` is set.
- Engine host ports are unique per node IP (`UNIQUE(node_ip, host_port)` is the allocator).
- A failed deployment is never retried automatically; Retry is the user's decision.
