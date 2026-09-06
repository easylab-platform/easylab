# ops-extension

Go single-binary extension server replacing the original Rust `executor` +
`sandbox-tools` + `builder` + `artifact-tools` services. No external binaries
(`kubectl`/`buildctl`) — it uses `client-go`, `moby/buildkit`, and
`gorilla/websocket` directly. Embeds an admin SPA (Svelte 5 + Tailwind 4,
shared zergx dark theme) served at `/`.

## Architecture

- **Session-scoped sandboxes**: the agent injects `_session`
  (`org:repo:bookmark`) into every tool call; ops-extension resolves the
  workspace against **jjlab only**, lazily creates/reuses the session's
  worker pod (k8s label `zergx/container=<key>`), and syncs the repo tree
  into it before execution.
- **Overlay sync**: only the worker's `sync/files` endpoint is used — repo
  files are refreshed to the bookmark head; files that exist only in the
  sandbox are never deleted. `sandbox-run` passes `rev` so the worker rejects
  execution on drift (`need_sync`), which triggers an automatic re-push.
- **Containerfile publish**: publishing runs each protocol's official CLI
  inside a buildkit build (no image export); base images resolve through
  buildkitd's proxy, uploads go to the artifact registry.

## Capabilities

- **NATS extension** (tools + discovery) for the agent via
  `forgejo.develop.10.199.64.20.nip.io/zergx/extension-sdk-go`.
- **HTTP API** for the zergx frontend (executor + builder surface).
- **Dynamic worker pods** via `client-go` (no `kubectl`).
- **Worker WebSocket RPC** via `gorilla/websocket`.
- **Image builds** via `moby/buildkit` (no `buildctl`).

## Dependency

`extension-sdk-go` is pulled from forgejo by git URL (not a local `replace`):

```
forgejo.develop.10.199.64.20.nip.io/zergx/extension-sdk-go v0.1.2
```

Build with private-module env:

```bash
GOINSECURE=forgejo.develop.10.199.64.20.nip.io \
GOPRIVATE=forgejo.develop.10.199.64.20.nip.io \
go build ./...
```

## HTTP surface

Executor: `/containers` (+ `/{cid}`, `/{cid}/exec`, `/{cid}/jobs*`, `/{cid}/kill/{job_id}`),
`/sandbox/read`, `/sandbox/write`, `/deploy`, `/infra/k8s/config`.

Builder: `/images/build`, `/containerfile-templates`, `/images`.

Admin/UI: `/status`, `/sessions`, `/packages`, `/publish-specs`,
`POST /packages/publish` — plus the embedded SPA at `/`.

## Frontend

`frontend/` — Svelte 5 (runes) + Vite + Tailwind 4 + bits-ui + lucide,
zod-validated API client (schemas are the single source of truth for types).
Same shared zergx dark theme as jjlab/artifact. Build with
`make frontend` (pnpm), embedded via `go:embed` — pages: Overview / Sessions /
Sandbox console / Builds / Packages / Tools.

## NATS tools (17)

**Sandbox (session-scoped, auto-synced)** — `sandbox-run`, `sandbox-read`,
`sandbox-write`, `sandbox-edit`, `sandbox-port`.

**Sandbox jobs** — `sandbox-job-list`, `sandbox-job-output`,
`sandbox-job-wait`, `sandbox-job-stdin`, `sandbox-job-kill`.

**Images** — `container-build`, `service-deploy`, `image-list`,
`list-containerfile-templates`.

**Packages** — `package-publish` (14 protocols), `list-registry-packages`.

**Repos** — `pull-git-repo`.

All sandbox tools resolve the workspace from `_session` (or legacy
`_org/_repo/_branch` args for debugging).

### package-publish (containerfile-based)

Publishing is done by running the protocol's official CLI inside a buildkit
build (no image export, only RUN side effects). The repo checkout from
jjlab (`GET /api/v1/repos/{org}/{repo}/{rev}/archive`) is the build
context; base images are pulled through the artifact OCI pull-through proxy,
so toolchain layers stay cached on the shared buildkitd.

- Manifest-driven protocols read name/version from the package manifest:
  `npm` (npm publish), `pypi` (python -m build + twine), `cargo`
  (cargo publish, sparse index), `rubygems` (gem build/push), `conan`
  (conan 2 create/upload), `pub` (dart pub publish), `helm`
  (helm package → chart-museum API).
- Explicit `name`/`version` required: `go` (module zip), `hex` (tar),
  `composer` (zip), `maven` (groupId + jar), `swift` (`scope.name` +
  archive-source zip), `generic` (raw `file`).
- `dockerfile_path` overrides the built-in template with a containerfile from
  the repo.

Cache-busting: every publish step references `$PUBLISH_TS` (fresh per
invocation) so a same-version re-publish never hits the RUN cache while
toolchain layers remain cached. Build failures return the build log tail so
the agent sees the CLI error.

## Config

| Env | Default |
| --- | ------- |
| `RUCODER_K8S_NAMESPACE` | `temp` |
| `RUCODER_WORKER_IMAGE`  | `zergx-artifact.temp.10.199.64.20.nip.io/zergx-worker:v0.0.1` |
| `RUCODER_BUILDKIT_ADDR` | `tcp://zergx-buildkitd.temp.svc.cluster.local:1234` |
| `RUCODER_ARTIFACT_URL`  | `http://zergx-artifact.temp.svc.cluster.local:80` (packages + OCI + metadata) |
| `RUCODER_ARTIFACT_TOKEN`| *(empty — anonymous)* token for artifact write auth (Bearer / X-NuGet-ApiKey / npm _authToken per protocol) |
| `RUCODER_JJ_SERVER_URL` | `http://zergx-jjlab.temp.svc.cluster.local:80` (archive + contents + clone) |
| `NATS_URL`              | `nats://nats.develop.svc.cluster.local:4222` |
| `RUCODER_PORT`          | `8080` |

> `RUCODER_REPO_MANAGER_URL` is still honored as a fallback for `RUCODER_JJ_SERVER_URL`.
> The old `RUCODER_REGISTRY` / `RUCODER_REGISTRY_URL` (zot + zergx-registry) are gone.
