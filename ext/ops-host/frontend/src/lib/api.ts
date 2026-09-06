// Typed API client for ops-extension's own HTTP face (same origin).
//
// Types are derived from zod schemas — the single source of truth: the schema
// validates every response at runtime and z.infer produces the TS type, so a
// backend shape change fails loudly instead of silently mistyping.
import { z } from 'zod'

const BASE = '/api/v1'

// ---------- schemas ----------

const StatusDep = z.object({
  name: z.string(),
  ok: z.boolean(),
  status: z.number().optional(),
  error: z.string().optional(),
})

export const StatusSchema = z.object({
  ok: z.boolean(),
  version: z.string(),
  sandboxes: z.number(),
  deps: z.array(StatusDep),
})

export const SandboxSchema = z.object({
  container_id: z.string(),
  session: z.string(),
  pod_name: z.string(),
  status: z.string(),
  worker_url: z.string(),
  pod_ip: z.string(),
  synced_rev: z.string(),
})

export const DeploymentSchema = z.object({
  name: z.string(),
  image: z.string(),
  replicas: z.number(),
  ready: z.number(),
  namespace: z.string(),
  age: z.string(),
  ports: z.array(z.number()),
  session: z.string().optional(),
  resources: ResourceRequestSchema,
})

export const ResourcePairSchema = z.object({
  cpu: z.string().optional(),
  memory: z.string().optional(),
})

export const ResourceRequestSchema = z.object({
  requests: ResourcePairSchema.optional(),
  limits: ResourcePairSchema.optional(),
})

export const PodSchema = z.object({
  name: z.string(),
  ip: z.string(),
  phase: z.string(),
  ready: z.boolean(),
  image: z.string(),
  age: z.string(),
  restarts: z.number(),
})

export const DeploymentStatusSchema = z.object({
  observed_generation: z.number(),
  updated_replicas: z.number(),
  ready_replicas: z.number(),
  available_replicas: z.number(),
  unavailable_replicas: z.number(),
  conditions: z.array(z.string()),
})

export const JobSchema = z.object({
  id: z.string(),
  command: z.string(),
  state: z.string(),
  exit_code: z.number(),
  started_at: z.number().nullable(),
  finished_at: z.number().nullable(),
})

export const ExecResultSchema = z.object({
  exit_code: z.number().optional(),
  output: z.string().optional(),
  backgrounded: z.boolean().optional(),
  job_id: z.string(),
})

export const FileReadSchema = z.object({
  ok: z.boolean(),
  content: z.string().optional(),
  error: z.string().optional(),
})

export const JobOutputSchema = z.object({
  lines: z.array(z.string()),
  done: z.boolean(),
  total_lines: z.number(),
})

export const PkgSchema = z.object({
  format: z.string(),
  repository: z.string(),
  versions: z.array(z.string()),
})

export const PublishSpecSchema = z.object({
  protocol: z.string(),
  args: z.array(z.string()),
  required: z.array(z.string()),
})

export const TemplateSchema = z.object({
  name: z.string(),
  content: z.string(),
})

export const BuildSchema = z.object({
  id: z.string(),
  kind: z.string(),
  tag: z.string(),
  state: z.string(),
  image: z.string().optional(),
  error: z.string().optional(),
  started_at: z.string(),
  finished_at: z.string().nullable().optional(),
  log_lines: z.number(),
})

export const HelmReleaseSchema = z.object({
  name: z.string(),
  namespace: z.string(),
  version: z.number(),
  status: z.string(),
  description: z.string().optional(),
  chart: z.string(),
  chart_version: z.string(),
  app_version: z.string(),
})

// ---------- inferred types ----------

export type Status = z.infer<typeof StatusSchema>
export type Sandbox = z.infer<typeof SandboxSchema>
export type Deployment = z.infer<typeof DeploymentSchema>
export type Pod = z.infer<typeof PodSchema>
export type DeploymentStatus = z.infer<typeof DeploymentStatusSchema>
export type Job = z.infer<typeof JobSchema>
export type ExecResult = z.infer<typeof ExecResultSchema>
export type Pkg = z.infer<typeof PkgSchema>
export type PublishSpec = z.infer<typeof PublishSpecSchema>
export type Template = z.infer<typeof TemplateSchema>
export type HelmRelease = z.infer<typeof HelmReleaseSchema>
export type Build = z.infer<typeof BuildSchema>

// ---------- fetch plumbing ----------

async function req<T>(path: string, schema: z.ZodType<T>, init?: RequestInit): Promise<T> {
  const r = await fetch(`${BASE}${path}`, init)
  const text = await r.text()
  let raw: unknown
  try {
    raw = text ? JSON.parse(text) : {}
  } catch {
    throw new Error(`HTTP ${r.status}: ${text.slice(0, 200)}`)
  }
  if (!r.ok) {
    const e = (raw as { error?: string }).error
    throw new Error(e ?? `HTTP ${r.status}`)
  }
  return schema.parse(raw)
}

function jsonInit(method: string, body?: unknown): RequestInit {
  return {
    method,
    headers: { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  }
}

const enc = encodeURI

/** WebSocket URL for an ops-extension path (same origin as the SPA). */
export function wsUrl(path: string): string {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
  return `${proto}//${location.host}${path}`
}

/** /sandboxes/{session}/ws (RPC + job.completed broadcast) URL. */
export function sandboxWsUrl(session: string): string {
  return wsUrl(`${BASE}/sandboxes/${session}/ws`)
}

/** /sandboxes/{session}/ws/job?job_id= (per-job SSE stream) URL. */
export function sandboxJobStreamUrl(session: string, jobId: string): string {
  return `${BASE}/sandboxes/${session}/ws/job?job_id=${encodeURI(jobId)}`
}

/** /builds/{id}/stream (SSE build log stream) URL. */
export function buildStreamUrl(id: string): string {
  return `${BASE}/builds/${enc(id)}/stream`
}

// ---------- api ----------

export const api = {
  status: () => req('/status', StatusSchema),

  sandboxes: () =>
    req('/sandboxes', z.object({ sandboxes: z.array(SandboxSchema) })),

  sandbox: (session: string) =>
    req(
      `/sandboxes/${session}`,
      z.object({
        sandbox: SandboxSchema,
        deployments: z.array(DeploymentSchema),
      }),
    ),

  deployments: () =>
    req('/deployments', z.object({ deployments: z.array(DeploymentSchema) })),

  deleteDeployment: (name: string) =>
    req(`/deployments/${enc(name)}`, z.object({ ok: z.boolean() }), { method: 'DELETE' }),

  deploymentPods: (name: string) =>
    req(`/deployments/${enc(name)}/pods`, z.object({ pods: z.array(PodSchema) })),

  deploymentStatus: (name: string) =>
    req(`/deployments/${enc(name)}/status`, DeploymentStatusSchema),

  deploymentRestart: (name: string) =>
    req(`/deployments/${enc(name)}/restart`, z.object({ ok: z.boolean() }), jsonInit('POST', {})),

  deploymentScale: (name: string, replicas: number) =>
    req(
      `/deployments/${enc(name)}/scale`,
      z.object({ ok: z.boolean(), replicas: z.number().optional() }),
      jsonInit('POST', { replicas }),
    ),

  deploymentRollback: (name: string, revision = 0) =>
    req(
      `/deployments/${enc(name)}/rollback`,
      z.object({ ok: z.boolean() }),
      jsonInit('POST', { revision }),
    ),

  deploymentEvents: (name: string) =>
    req(
      `/deployments/${enc(name)}/events`,
      z.object({ events: z.array(z.object({ reason: z.string(), message: z.string(), type: z.string(), age: z.string() })) }),
    ),

  deploymentRevisions: (name: string) =>
    req(
      `/deployments/${enc(name)}/revisions`,
      z.object({
        revisions: z.array(
          z.object({ revision: z.number(), image: z.string(), replicas: z.number(), ready: z.number(), age: z.string() }),
        ),
      }),
    ),

  helmReleases: () =>
    req('/helm/releases', z.object({ releases: z.array(HelmReleaseSchema) })),

  helmStatus: (name: string) =>
    req(`/helm/releases/${enc(name)}/status`, z.object({ release: HelmReleaseSchema })),

  helmValues: (name: string) =>
    req(`/helm/releases/${enc(name)}/values`, z.object({ values: z.record(z.string(), z.unknown()) })),

  helmInstall: (b: {
    release_name: string
    chart?: string
    version?: string
    values?: Record<string, unknown>
    org?: string
    repo?: string
    bookmark?: string
    chart_path?: string
  }) =>
    req('/helm/install', z.object({ ok: z.boolean(), build_id: z.string().optional(), error: z.string().optional() }), jsonInit('POST', b)),

  helmUninstall: (name: string) =>
    req(`/helm/releases/${enc(name)}`, z.object({ ok: z.boolean() }), { method: 'DELETE' }),

  helmRollback: (name: string, revision = 0) =>
    req(`/helm/releases/${enc(name)}/rollback`, z.object({ ok: z.boolean() }), jsonInit('POST', { revision })),

  deploy: (b: {
    name: string
    image: string
    replicas?: number
    port?: number
    env?: Record<string, string>
    session?: string
    resources?: {
      requests?: { cpu?: string; memory?: string }
      limits?: { cpu?: string; memory?: string }
    }
  }) =>
    req(
      '/deployments',
      z.object({ ok: z.boolean(), name: z.string().optional(), error: z.string().optional() }),
      jsonInit('POST', b),
    ),

  deleteSandbox: (session: string) =>
    req(`/sandboxes/${session}`, z.object({ ok: z.boolean() }), { method: 'DELETE' }),

  exec: (session: string, command: string) =>
    req(`/sandboxes/${session}/exec`, ExecResultSchema, jsonInit('POST', { command })),

  readFile: (session: string, path: string) =>
    req(`/sandboxes/${session}/read`, FileReadSchema, jsonInit('POST', { path })),

  writeFile: (session: string, path: string, content: string) =>
    req(
      `/sandboxes/${session}/write`,
      z.object({ ok: z.boolean(), path: z.string().optional(), error: z.string().optional() }),
      jsonInit('POST', { path, content }),
    ),

  jobs: (session: string) =>
    req(`/sandboxes/${session}/jobs`, z.object({ jobs: z.object({ jobs: z.array(JobSchema) }) })),

  jobOutput: (session: string, jobId: string) =>
    req(`/sandboxes/${session}/jobs/${enc(jobId)}/output`, JobOutputSchema),

  jobKill: (session: string, jobId: string) =>
    req(
      `/sandboxes/${session}/jobs/${enc(jobId)}/kill`,
      z.object({ ok: z.boolean(), result: z.unknown().optional() }),
      jsonInit('POST'),
    ),

  images: () => req('/images', z.object({ repositories: z.array(z.string()) })),

  buildImage: (b: { org: string; repo: string; bookmark: string; tag: string; dockerfile?: string; no_cache?: boolean }) =>
    req(
      '/images/build',
      z.object({ ok: z.boolean(), build_id: z.string().optional(), error: z.string().optional() }),
      jsonInit('POST', b),
    ),

  buildRaw: (b: { dockerfile: string; tag: string; no_cache?: boolean }) =>
    req(
      '/images/build',
      z.object({ ok: z.boolean(), build_id: z.string().optional(), error: z.string().optional() }),
      jsonInit('POST', { ...b, raw: true }),
    ),

  builds: () => req('/builds', z.object({ builds: z.array(BuildSchema) })),

  build: (id: string) =>
    req(
      `/builds/${enc(id)}`,
      z.object({ build: BuildSchema, logs: z.array(z.object({ stream: z.string(), line: z.string() })) }),
    ),

  templates: () => req('/containerfile-templates', z.object({ templates: z.array(TemplateSchema) })),

  packages: () => req('/packages', z.object({ packages: z.array(PkgSchema) })),

  publishSpecs: () => req('/publish-specs', z.object({ specs: z.array(PublishSpecSchema) })),

  publish: (b: Record<string, string>) =>
    req(
      '/packages/publish',
      z.object({ ok: z.boolean(), build_id: z.string().optional(), error: z.string().optional() }),
      jsonInit('POST', b),
    ),

  /** NATS tool bridge (verification instances only, ZERGX_DISABLE_NATS=1). */
  callTool: (name: string, args: Record<string, unknown>) =>
    req(
      `/tools/${encodeURIComponent(name)}`,
      z.object({ ok: z.boolean(), result: z.string().optional(), error: z.string().optional() }),
      jsonInit('POST', args),
    ),
}
