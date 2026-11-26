# oakestra-openai-api

An [Oakestra](https://oakestra.io) addon that serves LLMs behind one OpenAI-compatible endpoint.

It is the manual sibling of the AI coordinator: nothing is planned or placed for you. You define
models (a ModelPack OCI reference plus an engine image, vLLM by default), pick in the dashboard
which node each one runs on, and the addon deploys the engine there through Oakestra and waits
until it answers. Clients call `/v1/chat/completions` with an API key. `model` is a model name, or
`auto` to let a scheduler strategy pick among the models you marked as eligible.

## Architecture

```
 Browser -> dashboard nginx -> addonui-openai-api (nginx, /api -> /admin) --+
                                                                            v (admin listener :8081)
 External clients -- :8095 /v1 (API key) --------------------------->  root_ooa  (Go binary, SQLite in /data)
                                                                        deploy loop -> Oakestra client -> system manager
                                                                        gateway + scheduler -> engines
 ===================== engines are reached at http://<node ip>:<host port> =====================
 WORKERS    Oakestra service "<model>" per deployment, pinned to the chosen node, port 18000-18999
            model weights mounted by the modelpack CSI driver at /model
```

The backend has two listeners. The public one (container port 8080) serves `/v1/*`, `/healthz` and
`/readyz`. The admin one (container port 8081) serves `/admin/v1` for the dashboard UI; the UI
container reaches it as `root_ooa:8081` and clients never should.

Every deployment is a row with a state. A loop runs every `OOA_POLL_INTERVAL` (and whenever
something kicks it) and does one idempotent step per row: create the Oakestra service, wait for the
instance, probe the engine. There are no plans and no automatic retries.

The gateway checks the API key, looks up the ready deployments for the requested model, rewrites
`model` to the served name, sets the engine's own key and streams the answer back. The scheduler is
only involved for `auto`: a strategy ranks the ready candidates and the gateway walks that order if
an engine cannot be reached. The default strategy is `least-inflight`.

Addon containers are not on the Oakestra overlay, so every engine is published through the SLA port
mapping on a host port of its node and the gateway calls `http://<node ip>:<host port>`. Each
deployment gets its own engine key, otherwise the open host port would bypass your API keys.

Clients never see nodes, clusters, deployments or engine keys. `/v1` responses carry none of that
unless `OOA_DEBUG_HEADERS=true`.

## Quick start (demo on a laptop)

The demo runs against a fake Oakestra (`ooa-fakeoak`) with three simulated nodes, one of them
without the modelpack CSI driver, and fake engines that echo canned text. Nothing real is deployed
and no model is downloaded.

Prerequisites: Docker with Compose, and a checkout of
[dashboard-v2](https://github.com/oakestra/dashboard-v2) as a sibling directory
`../dashboard-v2` (or set `DASHBOARD_DIR` to its path).

```sh
# 1. The dashboard with its mock system manager and mock addons manager
cd ../dashboard-v2
docker compose -f compose.dev.yml up -d --build

# 2. This addon: fake Oakestra, backend, UI container, and a one-shot that registers the addon
cd ../oakestra-openai-api
docker compose up -d --build
```

Then open <http://localhost:8080> and log in as `Admin` with the password `Admin` (the dashboard
mock also knows `alice`, `bob`, `carol` and `dave`, password equal to the user name; see
`docs/demo.md` in dashboard-v2).

1. Open **OpenAI API** in the sidebar, then **Models**, and create a model: a name such as
   `demo-small`, any reference such as `registry.example.com/models/demo:1b`, memory 4096 MB.
2. Open **Placement**. It is a canvas you can pan (drag the background or scroll) and zoom
   (pinch, Ctrl/Cmd-scroll or the buttons), with the worker nodes on it and a floating Models
   box. Drag the model from the box onto a node. The node's free memory and GPUs update as you
   plan, and a node that cannot take the model says why. Nothing is deployed until you press
   **Apply**; **Discard** drops the plan. To take a model off a node, use the x on it or drag
   it back to the box, then apply.
3. Open **API keys**, create a key and copy it. It is shown once.
4. Call the endpoint (port 8095 is the public listener):

```sh
export OOA_KEY=sk-ooa-...

curl http://localhost:8095/v1/chat/completions \
  -H "Authorization: Bearer $OOA_KEY" -H "Content-Type: application/json" \
  -d '{"model":"demo-small","messages":[{"role":"user","content":"Hello"}]}'

# streaming
curl -N http://localhost:8095/v1/chat/completions \
  -H "Authorization: Bearer $OOA_KEY" -H "Content-Type: application/json" \
  -d '{"model":"demo-small","stream":true,"messages":[{"role":"user","content":"Hello"}]}'

# let the scheduler choose among models marked "eligible for auto"
curl http://localhost:8095/v1/chat/completions \
  -H "Authorization: Bearer $OOA_KEY" -H "Content-Type: application/json" \
  -d '{"model":"auto","messages":[{"role":"user","content":"Hello"}]}'
```

Stop everything with `docker compose down -v` here and in `../dashboard-v2`. `make smoke` runs the
same flow without a browser against the running stack.

## Using the API

Base URL: `http://<host>:8095/v1` (or your `OOA_PUBLIC_URL`). Every `/v1` call needs
`Authorization: Bearer sk-ooa-...`.

| Route | Notes |
|---|---|
| `POST /v1/chat/completions` | streaming (SSE) supported |
| `POST /v1/completions` | streaming supported |
| `POST /v1/embeddings` | needs an engine that serves embeddings |
| `GET /v1/models` | models with at least one ready deployment, plus `auto` when some eligible model is ready |
| `GET /v1/models/{id}` | one of the above, else 404 `model_not_found` |
| `GET /healthz`, `GET /readyz` | no key needed |

`model` is a model name or `auto`. A name routes to that model's ready deployments, least busy
first. `auto` routes among the ready deployments of models with "eligible for auto" set, ranked by
the configured strategy; if the strategy fails or takes longer than 2 seconds the gateway falls back
to `least-inflight`. If connecting to an engine fails before any response, the next candidate is
tried, up to `OOA_MAX_ATTEMPTS`.

Errors use the OpenAI shape `{"error":{"message","type","param","code"}}`:

| Status | `code` | Meaning |
|---|---|---|
| 401 | `invalid_api_key` | missing, wrong or revoked key |
| 400 | `invalid_json` | body is not a JSON object |
| 400 | `missing_required_parameter` | no `model` |
| 413 | `request_too_large` | body over `OOA_MAX_BODY_BYTES` |
| 404 | `model_not_found` | no such model |
| 404 | `not_found` | unknown `/v1` route |
| 503 | `no_deployment_available` | the model exists (or you asked for `auto`) but nothing is ready |
| 502 | `upstream_unreachable` | every tried engine failed to connect |

## Models page

| Field | Meaning |
|---|---|
| Name | The value clients pass as `model`. Unique; `auto` is reserved. Also the name the engine serves. |
| ModelPack reference | OCI reference of the model, passed to the CSI driver. |
| Engine image | Container image of the engine. Empty means `OOA_DEFAULT_ENGINE_IMAGE` (vLLM). |
| Extra engine arguments | One per line, appended to the engine command. |
| Command override | Optional argv, one element per line, for engines other than vLLM. |
| Memory, GPUs | Resources requested in the SLA. Memory is entered in GB or MB. |
| Eligible for auto | Whether `auto` may route to this model. |

Without a command override the engine runs
`vllm serve /model --served-model-name <name> --port 8000 --api-key <engine key>` plus your extra
arguments. The container port defaults to 8000. With an override, these placeholders are replaced in
every element: `{model_path}` (the mount path, `OOA_MODEL_MOUNT_PATH`), `{name}`, `{port}` and
`{api_key}`. The override exists because the SLA `cmd` replaces the image's entrypoint and command
entirely, so an image alone is not enough for an engine whose server is not started by default.
Your command must serve the OpenAI API on `{port}`, answer `/health` and `/v1/models`, and require
`{api_key}` as bearer token.

While a model has deployments only "eligible for auto" and labels can be edited. A model can be
deleted once its deployments are removed.

## Deployment states

| State | Meaning |
|---|---|
| `pending` | Row created. The addon creates (or adopts) the Oakestra service and deploys an instance. |
| `scheduling` | Waiting for Oakestra to report the instance RUNNING. This includes the model pull. Times out after `OOA_SCHEDULE_TIMEOUT`. |
| `starting` | The container runs; waiting for `/health` and `/v1/models`. Times out after `OOA_STARTUP_TIMEOUT`. |
| `ready` | Serving. Probed every tick; three failures in a row make it `unhealthy`. |
| `unhealthy` | Not routed. Becomes `ready` again on the first successful probe. |
| `failed` | Stays until you act. The error is shown. |
| `deleting` | The Oakestra service is being deleted, then the row. |

Retry is only offered for failed deployments. It clears the error and starts again from `pending`.
Remove deletes the Oakestra service and then the deployment, in any state. Nothing is ever removed
because a node vanished from a listing; if the node is gone, the deployment fails or turns unhealthy
and waits for you.

## Configuration

All variables are optional except the service account. Durations use Go syntax (`5s`, `15m`).

| Variable | Default | Meaning |
|---|---|---|
| `OOA_LISTEN` | `:8080` | Public listener (`/v1`, `/healthz`). |
| `OOA_ADMIN_LISTEN` | `:8081` | Admin listener (`/admin/v1`). Must differ from `OOA_LISTEN`. Only the UI container should reach it. |
| `OOA_PUBLIC_URL` | empty | Base URL clients use for `/v1`, shown in the UI. The UI falls back to its own origin. |
| `OOA_DATA_DIR` | `/data` | Directory of the SQLite database. |
| `OOA_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `OOA_OAKESTRA_API` | `http://system_manager:10000` | System manager base URL. |
| `OOA_OAKESTRA_USER` | none, required | Service account user. |
| `OOA_OAKESTRA_PASSWORD` | none, required | Service account password. |
| `OOA_OAKESTRA_ORG` | empty | Organization name at login; empty means root. |
| `OOA_CLUSTER_ABSTRACTOR_PORT` | `11012` | Port of each cluster's resource abstractor, reached at the cluster IP. |
| `OOA_CLUSTER_ABSTRACTOR_OVERRIDES` | empty | Abstractor base URL per cluster name or id, as `name=url,name2=url2`, for clusters whose advertised IP is not reachable. |
| `OOA_APP_NAME` | `openaiapi` | Name of the one Oakestra application that owns every engine service. `^[a-zA-Z0-9]{1,32}$`. |
| `OOA_APP_NS` | `ooa` | Namespace of that application. Same pattern. |
| `OOA_ENGINE_PORT_RANGE` | `18000-18999` | Host ports engines are published on, per node IP. |
| `OOA_DEFAULT_ENGINE_IMAGE` | `vllm/vllm-openai:v0.29.0` | Prefilled engine image. Pinned because the default argv is written against that vLLM. |
| `OOA_MODEL_CSI_DRIVER` | `model.csi.modelpack.org` | CSI driver name. Must equal the name the driver reports on the nodes. |
| `OOA_MODEL_CSI_REF_KEY` | `model.csi.modelpack.org/reference` | Volume attribute that carries the OCI reference. |
| `OOA_MODEL_MOUNT_PATH` | `/model` | Where the model appears inside the engine. Absolute path. |
| `OOA_POLL_INTERVAL` | `5s` | Period of the deployment loop (minimum 100ms). |
| `OOA_SCHEDULE_TIMEOUT` | `15m` | Wait for Oakestra to report RUNNING, including the model pull. |
| `OOA_STARTUP_TIMEOUT` | `30m` | Wait for the engine to answer its health probe. |
| `OOA_SCHEDULER` | `least-inflight` | Strategy for `auto`, used to seed the setting on first start. The Settings page changes it afterwards. |
| `OOA_MAX_ATTEMPTS` | `3` | Candidates tried per request when connecting fails before any response. |
| `OOA_MAX_BODY_BYTES` | `16MiB` | Cap for `/v1` request bodies. Plain bytes or a `KiB`/`MiB`/`GiB` suffix. |
| `OOA_DEBUG_HEADERS` | `false` | Adds deployment and node headers to `/v1` responses. Reveals topology. |
| `OOA_AUTH_URL` | `http://system_manager:10000/api` | API base used to validate dashboard tokens (`GET <url>/user/<sub>`). |
| `OOA_ADMIN_AUTH` | `introspect` | `introspect`, or `none` which treats every admin request as Admin. Tests only. |

The server logs a warning at start for `OOA_ADMIN_AUTH=none`, for Admin/Admin as service account
and for `OOA_DEBUG_HEADERS=true`. `deploy/env.example` lists the same variables.

The demo's fake Oakestra (`ooa-fakeoak`) is configured by flags or `OOA_FAKEOAK_<FLAG>` variables:
`-nodes`, `-clusters`, `-node-mem-mb`, `-node-used-mb`, `-arch`, `-gpus`, `-vram-mb`, `-node-ip`,
`-cluster-ip`, `-no-modelpack`, `-user`, `-password`, `-spawn-engines`, `-engine-bind`,
`-public-url`, `-listen`. See `cmd/ooa-fakeoak/doc.go`.

## Writing a custom scheduler strategy

Strategies live in `internal/scheduler`, which imports only the standard library. To add one, create
a file there, implement `Strategy` and call `Register` from `init`. The gateway and the admin API
do not need to change.

```go
package scheduler

import (
	"context"
	"encoding/json"
	"strings"
)

func init() {
	Register("tiered", func(cfg json.RawMessage, d Deps) (Strategy, error) {
		var c struct {
			Classifier string `json:"classifier"` // name of a deployed model
		}
		if err := json.Unmarshal(cfg, &c); err != nil {
			return nil, err
		}
		return &tiered{classifier: c.Classifier, deps: d}, nil
	})
}

type tiered struct {
	classifier string
	deps       Deps
}

func (*tiered) Name() string { return "tiered" }

// Short prompts go to models labelled tier=small. A decision model decides the rest.
func (s *tiered) Rank(ctx context.Context, r Request, c []Candidate) ([]Candidate, error) {
	tier := "small"
	if r.ApproxTokens > 500 && len(r.Messages) > 0 {
		answer, err := s.deps.Complete(ctx, s.classifier, []Message{
			{Role: "system", Content: "Answer with one word: small or large."},
			{Role: "user", Content: r.Messages[len(r.Messages)-1].Content},
		})
		if err != nil {
			return nil, err // the gateway falls back to least-inflight
		}
		tier = strings.TrimSpace(answer)
	}
	var matching []Candidate
	for _, cand := range c {
		if cand.Labels["tier"] == tier {
			matching = append(matching, cand)
		}
	}
	return RankLeastInflight(matching), nil
}
```

`Candidate.Labels` are the labels of the model; `Candidate.InFlight` and `TTFT` are live numbers.
`Deps.Complete` runs a non-streaming chat completion on a ready model of your choice. `Rank` returns
candidates best first and may drop some; an error or an empty result makes the gateway use
`least-inflight`, and a call is cut off after 2 seconds. `Rank` must not modify its input.

Select it by name on the **Settings** page, or set `OOA_SCHEDULER=tiered` before the first start
(afterwards the stored setting wins). The strategy's JSON config is handed to the factory as `cfg`.
Add a test next to the file; strategies are plain functions over `Request` and `Candidate`.

## Running on a real Oakestra

1. Create a dedicated Oakestra user for the addon. The deploy loop runs
   without a logged-in user and dashboard tokens expire after 10 minutes, so the addon logs in with
   its own credentials (`OOA_OAKESTRA_USER`, `OOA_OAKESTRA_PASSWORD`, `OOA_OAKESTRA_ORG`).
2. Edit the placeholders in `oakestra-addon.json` (`CHANGE_ME`), then
   submit it to the marketplace and install it:

   ```sh
   MARKETPLACE_URL=http://<root>:11102 ADDONS_MANAGER_URL=http://<root>:11101 \
     deploy/register-addon.py oakestra-addon.json
   ```

   The script uses only the Python standard library, is safe to run again and waits for the addon
   to become active. It publishes the `ooa` service (public `/v1` on host port 8095, data volume
   `ooa_data`) and the `ooa_ui` service. Images are published to
   `ghcr.io/robertjndw/oakestra-openai-api` (server), `...-addon-ui` and `...-fakeoak`.
3. Prepare each worker node that should run models:
   - Run the upstream CNCF modelpack CSI driver (`model.csi.modelpack.org`) on the worker, with
     `rshared` mount propagation for `/var/lib/oakestra/csi` so the mounts reach the engine
     container.
   - Register it with the node engine using exactly the name the driver reports, because the node
     engine mounts volumes by that name:
     `NodeEngine config csi add model.csi.modelpack.org unix://<driver socket>`.
   - Give the driver credentials for the registry that holds your models.
   - GPU nodes need the NVIDIA container toolkit.
   - Engines are published on host ports `18000-18999` (`OOA_ENGINE_PORT_RANGE`). Make them reachable
     from the root host and firewalled from everyone else.
4. In the placement page, only nodes that advertise the modelpack driver can be chosen.

### Known limitations

- The modelpack CSI path has not been tried on a real Oakestra node yet. It rests on reading the
  Oakestra sources: the SLA volume `config` map reaches the driver as VolumeContext. Everything else
  has run only against the fake Oakestra.
- The node engine gives a CSI publish 2 minutes. Pulling a large model for the first time can exceed
  that and fail the deployment. Pre-pull the model on the node, then press Retry.
- The model volume is mounted read-write.
- The SLA has no field for shared memory size, so engines that need a large `/dev/shm` cannot set it.
- The dashboard UI part needs the addons-v2 work in Oakestra and the dashboard (addon `ui` block
  support), which is not merged yet. The backend, the admin API and the `/v1` endpoint do not depend
  on it and work without it.
- Remove and Retry reuse the host port (and Retry the volume id) right away. Oakestra tears
  instances down asynchronously, so on a real node the new container can race the old one for the
  port or the volume and fail; press Retry again once the old instance is gone.
- Each engine's key is visible in the service's SLA `cmd` to anyone who can read the service in
  Oakestra.

## Development

```sh
make help       # list targets
make check      # tidy-check, fmt-check, lint, test-race: what CI runs
make test       # go test -shuffle=on ./...
make build      # every cmd/* into bin/
make vuln       # govulncheck
make fmt        # gofmt + goimports
make smoke      # end-to-end against the running demo stack
make up / make down   # start / stop this repo's compose stack
```

Frontend (`frontend/addon`, needs `../dashboard-v2` for the linked SDK packages, which need
`pnpm install` once in that checkout; CI pins its commit in `DASHBOARD_REF`):

```sh
cd frontend/addon
pnpm install
pnpm typecheck && pnpm test && pnpm build
pnpm dev        # Vite dev server on port 5182
```

Layout (Go module `github.com/robertjndw/oakestra-openai-api`, Go 1.26, `CGO_ENABLED=0`):

```
cmd/ooa-server        backend: config, wiring, two listeners, poll loop
cmd/ooa-fakeoak       fake Oakestra with simulated nodes and built-in fake engines
internal/config       env parsing, validation reporting all problems at once
internal/store        SQLite (pure Go), migrations, settings
internal/httpx        JSON helpers, error envelope, middleware, Serve
internal/oakestra     the only package that builds Oakestra requests (+ oakestratest fake)
internal/deploy       models, deployments, lifecycle loop, node listing, engine argv
internal/scheduler    Strategy interface, registry, least-inflight (stdlib only)
internal/gateway      /v1 proxy, endpoint table, in-flight counters, OpenAI errors
internal/auth         API keys (hashed), dashboard token introspection
internal/api          admin REST handlers
frontend/addon        dashboard UI addon (Module Federation remote, nginx container)
deploy/               register-addon.py, env.example, smoke.sh
```

Import direction: `cmd -> api -> {deploy, gateway, auth, scheduler} -> {oakestra, store, httpx,
config}`. `gateway` never imports `deploy`; `cmd` connects them with callbacks.

See `AGENTS.md` for conventions and invariants.
