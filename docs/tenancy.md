# Unified multi-tenant auth for EasyLab ↔ abcp-agent

Status: in progress. This document is the implementation checklist and design
record for giving the whole EasyLab stack one shared multi-tenant identity
model, bound to the abcp-agent's tenancy through a database record.

## Problem

EasyLab grew tenancy piecemeal and now has four defects:

1. **No bootstrap credential for the default tenant.** Once any user exists,
   the store is no longer "open" (`IsOpenInstance()` flips false), and every
   write path requires a *registered* bearer token. The chart ships
   `EASYVCS_TOKEN/EASYLAB_TOKEN/ARTIFACT_TOKEN=devtoken`, which is **not** a
   store token → every write 401s. (Root cause of the e2e failures.)
2. **Inconsistent credential scheme.** The embedded ops extension sends
   `Authorization: token <t>`; the gateway only strips `Bearer `. The repo
   extension (SDK) sends `Bearer`. Same credential, two spellings, one works.
3. **The agent binding is implicit and split.** `provisionTenant` calls the
   agent `AdminService.CreateTenant(id=slug)` and stores the returned bootstrap
   token in `tenants.agent_token`, but there is no explicit `agent_tenant`
   column, and the default tenant bypasses the DB (`EASYLAB_AGENT_TOKEN` env).
4. **Two tenant resolvers drift.** The gateway has its own
   `tenantOfRequest`/`tenantOfHeader`; artifactkit's `StoreAuth` has
   `TenantID` (fed by `TenantTokenStore`). `Principal.TenantID` is never
   populated by `easyvcsTokenStore`, so the registry's ownership layer always
   sees the default tenant.

## Target architecture

```
request --(Bearer|token)--> StoreAuth(easyvcs store) --> Principal{user, level, tenantID}
                                      |
                    tenants(DB)  <-- binding -->  abcp-agent tenant (id = slug)
                 id, slug, agent_tenant, agent_token
                                      |
      gateway fans tenant out to: repo / registry / sandbox / CI / agent forward
```

Principles:

- **easyvcs store is the single identity authority** (users, tokens, tenants,
  binding all live there).
- **`tenants` row is the single binding record**: easyvcs tenant ↔ agent
  tenant id + agent bootstrap token.
- **One resolver**: every surface (gateway REST + Connect, artifactkit
  registry, embedded extensions) resolves the tenant through the same
  `StoreAuth.TenantID` path; the loopback `X-Agent-Tenant` header is only an
  override for in-process extensions.

## Phases

### Phase 1 — restore correctness (easylab only)

- [ ] `cmd/easylab/authscheme.go`: one helper that extracts a credential from
      `Authorization: Bearer <t>` **or** `Authorization: token <t>` (and bare).
      Route `authOK`, `labPrincipal`, `tenantOfHeader` through it.
- [ ] `cmd/easylab/bootstrap.go`: on startup, when `EASYLAB_BOOTSTRAP_TOKEN`
      is set, idempotently ensure a default-tenant (id 1) operator user +
      write token equal to that value. Chart sets it to `devtoken`.
- [ ] `k8s/chart`: `gateway.bootstrapToken` + `EASYLAB_BOOTSTRAP_TOKEN` env.
- [ ] Fix lifecycle subject tenancy: publishers must use
      `abc.<tenant>.session.lifecycle.<kind>`; update the e2e harness.

### Phase 2 — make the binding explicit in the DB

- [ ] easyvcs store: add `AgentTenant` accessor + `agent_tenant` column;
      add `DeleteTenant`.
- [ ] `provisionTenant` writes both `agent_tenant` and `agent_token`.
- [ ] Startup bootstrap binds the default tenant's agent tenant/token into the
      DB (retire the `tid==1 → EASYLAB_AGENT_TOKEN` special case in
      `agentTokenForTenant`; env becomes a fallback only).
- [ ] `TenantService.DeleteTenant`: cascade delete (easyvcs rows + agent
      tenant) and `TenantService.UpdateTenant` propagates `disabled` to the
      agent.

### Phase 3 — converge on one tenant resolver

- [ ] `easyvcsTokenStore.LookupToken/LookupUsername` populate
      `Principal.TenantID` (currently always 0).
- [ ] `cmd/easylab/tenant.go` becomes a thin wrapper over
      `StoreAuth.TenantID` + the loopback `X-Agent-Tenant` override.
- [ ] Embedded ops extension uses the SDK (`easylabclient` + tenant
      interceptor) instead of raw HTTP with a divergent scheme.

### Phase 4 — agent tenancy lifecycle

- [ ] Create/update/delete tenants propagate to the agent `AdminService`.
- [ ] Document the contract (`docs/tenancy.md`) and the chart values.

## Verification

- `go build ./...` + `go test ./...` in easylab and easyvcs.
- Chart-deploy to `temp`, then in-pod e2e: the full tool sweep (39 passing),
  plus `service-deploy` / `sandbox-port` (the previously 401ing calls) and
  `repo/write` (the lifecycle-gated call).

## Rollout

- Smallest-first: Phase 1 unblocks the suite immediately; Phases 2–4 land as
  separate commits. easyvcs gets a new tag per store change and easylab bumps
  its `go.mod`.
