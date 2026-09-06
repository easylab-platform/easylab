<script lang="ts">
  import { onMount } from 'svelte'
  import { api, type Sandbox, type Deployment, type Pod } from '$lib/api'
  import Page from '$lib/components/Page.svelte'
  import * as Card from '$lib/components/ui/card'
  import { Badge } from '$lib/components/ui/badge'
  import * as Button from '$lib/components/ui/button'
  import { Trash2, RefreshCw, ChevronDown, ChevronRight } from '@lucide/svelte'

  let sandboxes: Sandbox[] = $state([])
  let error = $state('')
  let busy = $state('')
  let expanded = $state<Record<string, { deployments: Deployment[]; pods: Record<string, Pod[]> }>>({})

  async function refresh() {
    try {
      sandboxes = (await api.sandboxes()).sandboxes
      error = ''
    } catch (e) {
      error = String(e)
    }
  }

  async function remove(session: string) {
    busy = session
    try {
      await api.deleteSandbox(session)
      delete expanded[session]
      await refresh()
    } catch (e) {
      error = String(e)
    } finally {
      busy = ''
    }
  }

  async function toggle(session: string) {
    if (expanded[session]) {
      delete expanded[session]
      expanded = { ...expanded }
      return
    }
    try {
      const r = await api.sandbox(session)
      const pods: Record<string, Pod[]> = {}
      await Promise.all(
        r.deployments.map(async (d) => {
          pods[d.name] = (await api.deploymentPods(d.name)).pods
        }),
      )
      expanded = { ...expanded, [session]: { deployments: r.deployments, pods } }
    } catch (e) {
      error = String(e)
    }
  }

  onMount(() => {
    refresh()
    const t = setInterval(refresh, 10_000)
    return () => clearInterval(t)
  })
</script>

<Page title="Sandboxes" desc="One worker pod per session (org:repo:bookmark), plus the deployments each session owns">
  {#if error}<div class="mb-4 rounded-md border border-destructive/50 bg-destructive/10 p-3 text-sm text-destructive">{error}</div>{/if}
  <Card.Root>
    <Card.Content class="pt-6">
      {#if sandboxes.length === 0}
        <p class="py-8 text-center text-sm text-muted-foreground">
          No sandboxes. One is created lazily on the session's first sandbox tool call.
        </p>
      {:else}
        <div class="overflow-x-auto">
          <table class="w-full text-sm">
            <thead>
              <tr class="border-b text-left text-muted-foreground">
                <th class="py-2 pr-4 font-medium">Session</th>
                <th class="py-2 pr-4 font-medium">Pod</th>
                <th class="py-2 pr-4 font-medium">Status</th>
                <th class="py-2 pr-4 font-medium">Worker</th>
                <th class="py-2 pr-4 font-medium">Synced rev</th>
                <th class="py-2 pr-4 font-medium"></th>
              </tr>
            </thead>
            <tbody>
              {#each sandboxes as s (s.container_id)}
                <tr class="border-b last:border-0">
                  <td class="py-2 pr-4">
                    <button
                      class="flex items-center gap-1 font-mono text-xs hover:text-primary"
                      onclick={() => toggle(s.session)}
                    >
                      {#if expanded[s.session]}<ChevronDown class="size-3" />{:else}<ChevronRight class="size-3" />{/if}
                      {s.session}
                    </button>
                  </td>
                  <td class="py-2 pr-4 font-mono text-xs">{s.pod_name}</td>
                  <td class="py-2 pr-4">
                    <Badge variant={s.status === 'running' ? 'default' : 'secondary'}>{s.status}</Badge>
                  </td>
                  <td class="py-2 pr-4 font-mono text-xs text-muted-foreground">{s.pod_ip}</td>
                  <td class="py-2 pr-4 font-mono text-xs text-muted-foreground">
                    {s.synced_rev ? s.synced_rev.slice(0, 12) : '—'}
                  </td>
                  <td class="py-2 pr-4 text-right">
                    <Button.Root
                      variant="ghost"
                      size="icon"
                      disabled={busy === s.session}
                      onclick={() => remove(s.session)}
                      title="Delete pod + service"
                    >
                      <Trash2 class="size-4 text-destructive" />
                    </Button.Root>
                  </td>
                </tr>
                {#if expanded[s.session]}
                  <tr>
                    <td colspan="6" class="bg-muted/30 px-4 py-3">
                      {#if expanded[s.session].deployments.length === 0}
                        <p class="text-xs text-muted-foreground">No deployments owned by this session.</p>
                      {:else}
                        <div class="space-y-2">
                          {#each expanded[s.session].deployments as d (d.name)}
                            <div class="rounded border bg-card p-2">
                              <div class="flex items-center gap-2 text-xs">
                                <span class="font-mono">{d.name}</span>
                                <span class="font-mono text-muted-foreground">{d.image}</span>
                                <Badge variant={d.ready === d.replicas ? 'default' : 'secondary'}>{d.ready}/{d.replicas}</Badge>
                                <span class="text-muted-foreground">{d.age}</span>
                              </div>
                              <div class="mt-1.5 pl-3">
                                {#each (expanded[s.session].pods[d.name] ?? []) as p (p.name)}
                                  <div class="flex items-center gap-2 py-0.5 font-mono text-[11px] text-muted-foreground">
                                    <span>{p.name}</span>
                                    <span>{p.ip || '—'}</span>
                                    <span class={p.ready ? 'text-emerald-500' : 'text-destructive'}>{p.ready ? 'ready' : p.phase}</span>
                                    <span>restarts={p.restarts}</span>
                                    <span>{p.age}</span>
                                  </div>
                                {/each}
                              </div>
                            </div>
                          {/each}
                        </div>
                      {/if}
                    </td>
                  </tr>
                {/if}
              {/each}
            </tbody>
          </table>
        </div>
      {/if}
    </Card.Content>
  </Card.Root>
</Page>
