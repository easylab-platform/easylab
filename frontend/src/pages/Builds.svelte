<script lang="ts">
  import { onMount, onDestroy } from 'svelte'
  import { api, buildStreamUrl, type Template, type Build } from '$lib/api'
  import Page from '$lib/components/Page.svelte'
  import * as Card from '$lib/components/ui/card'
  import * as Button from '$lib/components/ui/button'
  import * as Input from '$lib/components/ui/input'
  import * as Select from '$lib/components/ui/native-select'
  import { Badge } from '$lib/components/ui/badge'
  import { Hammer, RefreshCw } from '@lucide/svelte'

  // ---- mode: repo (org/repo/bookmark) or raw (paste a Containerfile) ----
  let mode = $state<'repo' | 'raw'>('repo')
  let org = $state('')
  let repo = $state('')
  let session = $state('')
  let bookmark = $state('main')
  let tag = $state('')
  let dockerfile = $state('Dockerfile')
  let rawContent = $state('FROM alpine:3.20\n\nCMD ["sh", "-c", "echo hello"]\n')
  let noCache = $state(false)
  let building = $state(false)

  let images: string[] = $state([])
  let templates: Template[] = $state([])

  // ---- build history + live log ----
  let builds: Build[] = $state([])
  let activeBuildId = $state('')
  let logLines: { stream: string; line: string }[] = $state([])
  let activeState = $state('')
  let es: EventSource | null = null

  async function build() {
    building = true
    try {
      const r =
        mode === 'repo'
          ? await api.buildImage({ org, repo, bookmark, tag, dockerfile, no_cache: noCache })
          : await api.buildRaw({ dockerfile: rawContent, tag, no_cache: noCache })
      if (r.ok && r.build_id) {
        openStream(r.build_id)
      } else {
        logLines = [{ stream: 'error', line: r.error ?? 'failed' }]
      }
      await refreshBuilds()
    } catch (e) {
      logLines = [{ stream: 'error', line: String(e) }]
    } finally {
      building = false
    }
  }

  function openStream(id: string) {
    closeStream()
    activeBuildId = id
    activeState = 'running'
    logLines = []
    es = new EventSource(buildStreamUrl(id))
    es.addEventListener('log', (e) => {
      try {
        const d = JSON.parse((e as MessageEvent).data)
        logLines = [...logLines, d]
      } catch {}
    })
    es.addEventListener('state', (e) => {
      try {
        const d = JSON.parse((e as MessageEvent).data)
        activeState = d.state
      } catch {}
    })
    es.addEventListener('done', (e) => {
      try {
        const d = JSON.parse((e as MessageEvent).data)
        activeState = d.state
      } catch {}
      refreshBuilds()
      closeStream()
    })
    es.onerror = () => {
      // EventSource auto-reconnects; keep the stream open. When the server
      // closed after completion, onerror fires after 'done' and closeStream
      // already detached.
    }
  }

  function closeStream() {
    if (es) {
      es.close()
      es = null
    }
    activeBuildId = ''
  }

  async function refreshBuilds() {
    try {
      builds = (await api.builds()).builds
      images = (await api.images()).repositories
    } catch {}
  }

  async function viewBuild(id: string) {
    try {
      const r = await api.build(id)
      activeBuildId = id
      activeState = r.build.state
      logLines = r.logs
    } catch {}
  }

  function loadTemplate(t: Template) {
    rawContent = t.content
    mode = 'raw'
  }

  onMount(async () => {
    try {
      images = (await api.images()).repositories
      templates = (await api.templates()).templates
      builds = (await api.builds()).builds
    } catch {}
  })

  onDestroy(() => closeStream())
</script>

<Page title="Builds" desc="Build container images (from a repo or a pasted Containerfile) via buildkitd">
  <div class="grid gap-4 lg:grid-cols-3">
    <Card.Root class="lg:col-span-2">
      <Card.Header class="flex-row items-center justify-between">
        <Card.Title class="text-sm">Build image</Card.Title>
        <div class="flex rounded-md border">
          <button
            class="px-3 py-1 text-xs {mode === 'repo' ? 'bg-primary text-primary-foreground' : 'text-muted-foreground'}"
            onclick={() => (mode = 'repo')}
          >From repo</button
          >
          <button
            class="px-3 py-1 text-xs border-l {mode === 'raw' ? 'bg-primary text-primary-foreground' : 'text-muted-foreground'}"
            onclick={() => (mode = 'raw')}
          >Paste Containerfile</button
          >
        </div>
      </Card.Header>
      <Card.Content class="space-y-3">
        {#if mode === 'repo'}
          <Input.Root placeholder="session (org:repo:bookmark) — or fill below" bind:value={session} />
          <div class="grid grid-cols-2 gap-2">
            <Input.Root placeholder="org" bind:value={org} />
            <Input.Root placeholder="repo" bind:value={repo} />
            <Input.Root placeholder="bookmark (main)" bind:value={bookmark} />
            <Input.Root placeholder="tag" bind:value={tag} />
          </div>
          <Input.Root placeholder="dockerfile path" bind:value={dockerfile} />
        {:else}
          <Input.Root placeholder="tag" bind:value={tag} />
          <textarea
            class="h-64 w-full rounded-md bg-muted p-3 font-mono text-xs"
            placeholder="FROM alpine:3.20 ..."
            bind:value={rawContent}
          ></textarea>
        {/if}
        <div class="flex items-center gap-3">
          <Button.Root onclick={build} disabled={building || !tag || (mode === 'repo' && !session && (!org || !repo))}>
            <Hammer class="size-4" /> {building ? 'Submitting…' : 'Build + push'}
          </Button.Root>
          <label class="flex items-center gap-1.5 text-xs text-muted-foreground">
            <input type="checkbox" bind:checked={noCache} />
            no-cache
          </label>
          {#if activeBuildId}
            <span class="text-xs text-muted-foreground">
              build {activeBuildId.slice(0, 8)}
              <Badge variant={activeState === 'running' ? 'default' : activeState === 'done' ? 'secondary' : 'destructive'}>
                {activeState}
              </Badge>
            </span>
          {/if}
        </div>
        {#if logLines.length}
          <pre class="max-h-96 overflow-auto rounded-md bg-muted p-3 font-mono text-xs whitespace-pre-wrap">{logLines.map((l) => l.line).join('\n')}</pre>
        {/if}
      </Card.Content>
    </Card.Root>

    <Card.Root>
      <Card.Header><Card.Title class="text-sm">Containerfile templates</Card.Title></Card.Header>
      <Card.Content class="space-y-2">
        {#each templates as t (t.name)}
          <div class="flex items-center justify-between rounded-md border px-3 py-2">
            <code class="font-mono text-xs">{t.name}</code>
            <Button.Root size="sm" variant="outline" onclick={() => loadTemplate(t)}>use</Button.Root>
          </div>
        {/each}
      </Card.Content>
    </Card.Root>
  </div>

  <Card.Root class="mt-4">
    <Card.Header class="flex-row items-center justify-between">
      <Card.Title class="text-sm">Builds ({builds.length})</Card.Title>
      <Button.Root variant="ghost" size="icon" onclick={refreshBuilds}><RefreshCw class="size-4" /></Button.Root>
    </Card.Header>
    <Card.Content>
      {#if builds.length === 0}
        <p class="py-4 text-center text-sm text-muted-foreground">No builds yet.</p>
      {:else}
        <div class="overflow-x-auto">
          <table class="w-full text-sm">
            <thead>
              <tr class="border-b text-left text-muted-foreground">
                <th class="py-2 pr-4 font-medium">ID</th>
                <th class="py-2 pr-4 font-medium">Kind</th>
                <th class="py-2 pr-4 font-medium">Tag</th>
                <th class="py-2 pr-4 font-medium">State</th>
                <th class="py-2 pr-4 font-medium">Lines</th>
                <th class="py-2 pr-4 font-medium"></th>
              </tr>
            </thead>
            <tbody>
              {#each builds as b (b.id)}
                <tr class="border-b last:border-0">
                  <td class="py-2 pr-4 font-mono text-xs">{b.id.slice(0, 8)}</td>
                  <td class="py-2 pr-4 text-xs">{b.kind}</td>
                  <td class="py-2 pr-4 font-mono text-xs">{b.tag}</td>
                  <td class="py-2 pr-4">
                    <Badge variant={b.state === 'running' ? 'default' : b.state === 'done' ? 'secondary' : 'destructive'}>
                      {b.state}
                    </Badge>
                  </td>
                  <td class="py-2 pr-4 font-mono text-xs text-muted-foreground">{b.log_lines}</td>
                  <td class="py-2 pr-4 text-right">
                    <Button.Root size="sm" variant="outline" onclick={() => viewBuild(b.id)}>view</Button.Root>
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {/if}
    </Card.Content>
  </Card.Root>

  <Card.Root class="mt-4">
    <Card.Header><Card.Title class="text-sm">Registry images ({images.length})</Card.Title></Card.Header>
    <Card.Content>
      <div class="flex flex-wrap gap-1.5">
        {#each images as img (img)}
          <code class="rounded bg-muted px-1.5 py-0.5 font-mono text-xs">{img}</code>
        {/each}
      </div>
    </Card.Content>
  </Card.Root>
</Page>
