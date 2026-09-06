<script lang="ts">
  import { onMount } from 'svelte'
  import { api, type HelmRelease } from '$lib/api'
  import Page from '$lib/components/Page.svelte'
  import * as Card from '$lib/components/ui/card'
  import { Badge } from '$lib/components/ui/badge'
  import * as Button from '$lib/components/ui/button'
  import * as Input from '$lib/components/ui/input'
  import * as Textarea from '$lib/components/ui/textarea'
  import { Trash2, Rocket, ChevronDown, ChevronRight } from '@lucide/svelte'

  let releases: HelmRelease[] = $state([])
  let error = $state('')
  let busy = $state('')

  // install form
  let hRelease = $state('')
  let hChart = $state('')
  let hOrg = $state('')
  let hRepo = $state('')
  let hBookmark = $state('dev')
  let hChartPath = $state('')
  let hValues = $state('')
  let hResult = $state('')
  let installing = $state(false)

  let expanded = $state<Record<string, HelmRelease>>({})
  let expandedValues = $state<Record<string, string>>({})

  async function refresh() {
    try {
      releases = (await api.helmReleases()).releases
      error = ''
    } catch (e) {
      error = String(e)
    }
  }

  async function install() {
    installing = true
    hResult = ''
    let values: Record<string, unknown> | undefined
    if (hValues.trim()) {
      try {
        values = JSON.parse(hValues)
      } catch (e) {
        hResult = `invalid values JSON: ${String(e)}`
        installing = false
        return
      }
    }
    try {
      const res = await api.helmInstall({
        release_name: hRelease,
        chart: hOrg || hRepo ? undefined : hChart,
        org: hOrg || undefined,
        repo: hRepo || undefined,
        bookmark: hBookmark || undefined,
        chart_path: hChartPath || undefined,
        values,
      })
      hResult = `installing ${hRelease} (task ${res.build_id})`
      hRelease = ''
      hChart = ''
      hValues = ''
      setTimeout(refresh, 3000)
    } catch (e) {
      hResult = String(e)
    } finally {
      installing = false
    }
  }

  async function uninstall(name: string) {
    if (!confirm(`Uninstall release ${name}?`)) return
    busy = name
    try {
      await api.helmUninstall(name)
      delete expanded[name]
      await refresh()
    } catch (e) {
      error = String(e)
    } finally {
      busy = ''
    }
  }

  async function rollback(name: string) {
    busy = name
    try {
      await api.helmRollback(name, 0)
      await refresh()
    } catch (e) {
      error = String(e)
    } finally {
      busy = ''
    }
  }

  async function toggle(name: string) {
    if (expanded[name]) {
      delete expanded[name]
      expanded = { ...expanded }
      return
    }
    try {
      const status = await api.helmStatus(name)
      expanded = { ...expanded, [name]: status.release }
    } catch (e) {
      error = String(e)
    }
  }

  async function showValues(name: string) {
    try {
      const v = await api.helmValues(name)
      expandedValues = { ...expandedValues, [name]: JSON.stringify(v.values, null, 2) }
    } catch (e) {
      error = String(e)
    }
  }

  onMount(() => {
    refresh()
    const t = setInterval(refresh, 15_000)
    return () => clearInterval(t)
  })
</script>

<Page title="Helm releases" desc="Install a Helm chart as a release (repo checkout or chart ref)">
  {#if error}<div class="mb-4 rounded-md border border-destructive/50 bg-destructive/10 p-3 text-sm text-destructive">{error}</div>{/if}

  <Card.Root class="mb-4">
    <Card.Header><Card.Title class="text-sm">Install chart</Card.Title></Card.Header>
    <Card.Content class="space-y-3">
      <div class="grid grid-cols-2 gap-2">
        <Input.Root placeholder="release name" bind:value={hRelease} />
        <Input.Root placeholder="chart ref (name or path, if not using repo)" bind:value={hChart} />
        <Input.Root placeholder="org (repo mode)" bind:value={hOrg} />
        <Input.Root placeholder="repo (repo mode)" bind:value={hRepo} />
        <Input.Root placeholder="bookmark (dev)" bind:value={hBookmark} />
        <Input.Root placeholder="chart path in repo (.)" bind:value={hChartPath} />
      </div>
      <div>
        <div class="mb-1 text-xs text-muted-foreground">Values (JSON, optional)</div>
          <Textarea.Root class="h-32 font-mono text-xs" placeholder={"{ \"replicaCount\": 2 }"} bind:value={hValues} />
      </div>
      <div class="flex flex-wrap items-center gap-2">
        <Button.Root onclick={install} disabled={installing || !hRelease || (!hChart && (!hOrg || !hRepo))}>
          <Rocket class="size-4" /> {installing ? 'Installing…' : 'Install'}
        </Button.Root>
        {#if hResult}<span class="text-xs text-muted-foreground">{hResult}</span>{/if}
      </div>
    </Card.Content>
  </Card.Root>

  <Card.Root>
    <Card.Content class="pt-6">
      {#if releases.length === 0}
        <p class="py-8 text-center text-sm text-muted-foreground">No helm releases.</p>
      {:else}
        <div class="overflow-x-auto">
          <table class="w-full text-sm">
            <thead>
              <tr class="border-b text-left text-muted-foreground">
                <th class="py-2 pr-4 font-medium">Name</th>
                <th class="py-2 pr-4 font-medium">Chart</th>
                <th class="py-2 pr-4 font-medium">Version</th>
                <th class="py-2 pr-4 font-medium">Status</th>
                <th class="py-2 pr-4 font-medium"></th>
              </tr>
            </thead>
            <tbody>
              {#each releases as r (r.name)}
                <tr class="border-b last:border-0">
                  <td class="py-2 pr-4">
                    <button class="flex items-center gap-1 font-mono text-xs hover:text-primary" onclick={() => toggle(r.name)}>
                      {#if expanded[r.name]}<ChevronDown class="size-3" />{:else}<ChevronRight class="size-3" />{/if}
                      {r.name}
                    </button>
                  </td>
                  <td class="py-2 pr-4 font-mono text-xs text-muted-foreground">{r.chart} {r.chart_version}</td>
                  <td class="py-2 pr-4 font-mono text-xs text-muted-foreground">{r.version}</td>
                  <td class="py-2 pr-4">
                    <Badge variant={r.status === 'deployed' ? 'default' : 'secondary'}>{r.status}</Badge>
                  </td>
                  <td class="py-2 pr-4 text-right">
                    <div class="flex items-center justify-end gap-1">
                      <Button.Root variant="ghost" size="sm" disabled={busy === r.name} onclick={() => rollback(r.name)} title="Rollback to previous">
                        ↩
                      </Button.Root>
                      <Button.Root variant="ghost" size="sm" disabled={busy === r.name} onclick={() => showValues(r.name)} title="Show values">
                        values
                      </Button.Root>
                      <Button.Root variant="ghost" size="icon" disabled={busy === r.name} onclick={() => uninstall(r.name)} title="Uninstall">
                        <Trash2 class="size-4 text-destructive" />
                      </Button.Root>
                    </div>
                  </td>
                </tr>
                {#if expanded[r.name]}
                  <tr>
                    <td colspan="5" class="bg-muted/30 px-4 py-2 font-mono text-[11px] text-muted-foreground">
                      {expanded[r.name]?.description}
                    </td>
                  </tr>
                {/if}
                {#if expandedValues[r.name]}
                  <tr>
                    <td colspan="5" class="bg-muted/30 px-4 py-2">
                      <pre class="overflow-auto font-mono text-[11px]">{expandedValues[r.name]}</pre>
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
