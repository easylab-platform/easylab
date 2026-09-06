<script lang="ts">
  import { onMount } from 'svelte'
  import { api, type Status } from '$lib/api'
  import Page from '$lib/components/Page.svelte'
  import * as Card from '$lib/components/ui/card'
  import { Badge } from '$lib/components/ui/badge'
  import { Activity, Boxes, Server, CheckCircle2, XCircle } from '@lucide/svelte'

  let status: Status | null = $state(null)
  let error = $state('')

  async function refresh() {
    try {
      status = await api.status()
      error = ''
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

<Page title="Overview" desc="ops-extension service health">
  {#if error}
    <Card.Root>
      <Card.Content class="pt-6 text-destructive">{error}</Card.Content>
    </Card.Root>
  {:else if status}
    <div class="grid gap-4 md:grid-cols-4">
      <Card.Root>
        <Card.Header class="flex flex-row items-center justify-between">
          <Card.Title class="text-sm font-medium">Version</Card.Title>
          <Activity class="size-4 text-muted-foreground" />
        </Card.Header>
        <Card.Content><div class="text-2xl font-bold">{status.version}</div></Card.Content>
      </Card.Root>
      <Card.Root>
        <Card.Header class="flex flex-row items-center justify-between">
          <Card.Title class="text-sm font-medium">Sandboxes</Card.Title>
          <Boxes class="size-4 text-muted-foreground" />
        </Card.Header>
        <Card.Content><div class="text-2xl font-bold">{status.sandboxes}</div></Card.Content>
      </Card.Root>
      <Card.Root class="md:col-span-2">
        <Card.Header class="flex flex-row items-center justify-between">
          <Card.Title class="text-sm font-medium">Dependencies</Card.Title>
          <Server class="size-4 text-muted-foreground" />
        </Card.Header>
        <Card.Content class="flex flex-wrap gap-2">
          {#each status.deps as dep (dep.name)}
            <Badge variant={dep.ok ? 'default' : 'destructive'} class="gap-1.5">
              {#if dep.ok}<CheckCircle2 class="size-3" />{:else}<XCircle class="size-3" />{/if}
              {dep.name}
            </Badge>
          {/each}
        </Card.Content>
      </Card.Root>
    </div>
  {:else}
    <Card.Root><Card.Content class="pt-6 text-muted-foreground">Loading…</Card.Content></Card.Root>
  {/if}
</Page>
