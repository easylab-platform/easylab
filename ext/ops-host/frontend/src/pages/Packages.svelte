<script lang="ts">
  import { onMount } from 'svelte'
  import { api, type Pkg, type PublishSpec } from '$lib/api'
  import Page from '$lib/components/Page.svelte'
  import * as Card from '$lib/components/ui/card'
  import * as Button from '$lib/components/ui/button'
  import * as Input from '$lib/components/ui/input'
  import * as Select from '$lib/components/ui/native-select'
  import { Badge } from '$lib/components/ui/badge'
  import { Upload } from '@lucide/svelte'

  let specs: PublishSpec[] = $state([])
  let protocol = $state('npm')
  let session = $state('')
  let org = $state('')
  let repo = $state('')
  let name = $state('')
  let version = $state('')
  let file = $state('')
  let publishing = $state(false)
  let result = $state('')

  let packages: Pkg[] = $state([])
  let filter = $state('')

  const spec = $derived(specs.find((s) => s.protocol === protocol))

  async function refresh() {
    try {
      packages = (await api.packages()).packages
    } catch {}
  }

  async function publish() {
    publishing = true
    result = ''
    try {
      const body: Record<string, string> = {
        protocol,
        session,
        org,
        repo,
        name,
        version,
        file,
      }
      for (const k of Object.keys(body)) if (!body[k]) delete body[k]
      const r = await api.publish(body)
      result = r.ok ? `submitted build ${r.build_id ?? ''}` : (r.error ?? 'failed')
    } catch (e) {
      result = String(e)
    } finally {
      publishing = false
    }
  }

  const shown = $derived(
    filter
      ? packages.filter(
          (p) =>
            p.format.includes(filter) ||
            p.repository.includes(filter) ||
            p.versions.some((v) => v.includes(filter)),
        )
      : packages,
  )

  onMount(async () => {
    try {
      specs = (await api.publishSpecs()).specs
    } catch {}
    refresh()
  })
</script>

<Page title="Packages" desc="Artifact registry packages across all protocols">
  <div class="grid gap-4 lg:grid-cols-3">
    <Card.Root class="lg:col-span-1">
      <Card.Header><Card.Title class="text-sm">Publish</Card.Title></Card.Header>
      <Card.Content class="space-y-3">
        <Select.Root bind:value={protocol}>
          {#each specs as s (s.protocol)}
            <option value={s.protocol}>{s.protocol}</option>
          {/each}
        </Select.Root>
        <Input.Root placeholder="session (org:repo:bookmark)" bind:value={session} />
        <div class="grid grid-cols-2 gap-2">
          <Input.Root placeholder="org*" bind:value={org} />
          <Input.Root placeholder="repo*" bind:value={repo} />
          <Input.Root placeholder="name" bind:value={name} />
          <Input.Root placeholder="version" bind:value={version} />
        </div>
        <Input.Root placeholder="file (generic)" bind:value={file} />
        {#if spec?.required.length}
          <p class="text-xs text-muted-foreground">required: {spec.required.join(', ').toLowerCase()}</p>
        {:else}
          <p class="text-xs text-muted-foreground">manifest-driven — name/version read from the package manifest</p>
        {/if}
        <Button.Root onclick={publish} disabled={publishing || (!session && (!org || !repo))}>
          <Upload class="size-4" /> {publishing ? 'Publishing…' : 'Publish'}
        </Button.Root>
        {#if result}<pre class="rounded-md bg-muted p-3 font-mono text-xs whitespace-pre-wrap">{result}</pre>{/if}
      </Card.Content>
    </Card.Root>

    <Card.Root class="lg:col-span-2">
      <Card.Header class="flex-row items-center justify-between">
        <Card.Title class="text-sm">Packages ({shown.length})</Card.Title>
        <div class="w-48"><Input.Root placeholder="filter…" bind:value={filter} /></div>
      </Card.Header>
      <Card.Content>
        <div class="max-h-[28rem] space-y-1.5 overflow-auto">
          {#each shown as p (`${p.format}/${p.repository}`)}
            <div class="flex items-center gap-2 rounded-md border px-3 py-1.5">
              <Badge variant="secondary" class="font-mono text-[10px]">{p.format}</Badge>
              <span class="font-mono text-xs">{p.repository}</span>
              <span class="ml-auto font-mono text-[11px] text-muted-foreground">{p.versions.length} version(s)</span>
            </div>
          {/each}
        </div>
      </Card.Content>
    </Card.Root>
  </div>
</Page>
