<script lang="ts">
  import { onMount } from 'svelte'
  import { api, type Deployment, type Pod, type DeploymentStatus } from '$lib/api'
  import Page from '$lib/components/Page.svelte'
  import * as Card from '$lib/components/ui/card'
  import { Badge } from '$lib/components/ui/badge'
  import * as Button from '$lib/components/ui/button'
  import * as Input from '$lib/components/ui/input'
  import * as Select from '$lib/components/ui/native-select'
  import { Trash2, Rocket, ChevronDown, ChevronRight } from '@lucide/svelte'

  let deployments: Deployment[] = $state([])
  let error = $state('')

  // deploy form
  let images: string[] = $state([])
  let dName = $state('')
  let dImage = $state('')
  let dReplicas = $state(1)
  let dPort = $state(8080)
  let dSession = $state('')
  let dCpuRequest = $state('')
  let dMemRequest = $state('')
  let dCpuLimit = $state('')
  let dMemLimit = $state('')
  let dResult = $state('')
  let deploying = $state(false)

  let busy = $state('')
  let expanded = $state<Record<string, { pods: Pod[]; status: DeploymentStatus | null }>>({})
  let eventsFor = $state<Record<string, { reason: string; message: string; type: string; age: string }[]>>({})

  async function refresh() {
    try {
      deployments = (await api.deployments()).deployments
      if (!images.length) images = (await api.images()).repositories
      error = ''
    } catch (e) {
      error = String(e)
    }
  }

  async function deploy() {
    deploying = true
    dResult = ''
    const resources = {
      requests:
        dCpuRequest || dMemRequest
          ? { cpu: dCpuRequest || undefined, memory: dMemRequest || undefined }
          : undefined,
      limits:
        dCpuLimit || dMemLimit
          ? { cpu: dCpuLimit || undefined, memory: dMemLimit || undefined }
          : undefined,
    }
    try {
      await api.deploy({
        name: dName,
        image: dImage,
        replicas: dReplicas || 1,
        port: dPort || 8080,
        session: dSession || undefined,
        resources: resources.requests || resources.limits ? resources : undefined,
      })
      dResult = `deployed ${dName}`
      dName = ''
      dImage = ''
      dSession = ''
      dCpuRequest = dMemRequest = dCpuLimit = dMemLimit = ''
      await refresh()
    } catch (e) {
      dResult = String(e)
    } finally {
      deploying = false
    }
  }

  async function remove(name: string) {
    busy = name
    try {
      await api.deleteDeployment(name)
      delete expanded[name]
      await refresh()
    } catch (e) {
      error = String(e)
    } finally {
      busy = ''
    }
  }

  async function restart(name: string) {
    busy = name
    try {
      await api.deploymentRestart(name)
      await refresh()
    } catch (e) {
      error = String(e)
    } finally {
      busy = ''
    }
  }

  async function scale(name: string, replicas: number) {
    busy = name
    try {
      await api.deploymentScale(name, replicas)
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
      await api.deploymentRollback(name, 0)
      await refresh()
    } catch (e) {
      error = String(e)
    } finally {
      busy = ''
    }
  }

  async function showEvents(name: string) {
    busy = name
    try {
      const r = await api.deploymentEvents(name)
      eventsFor = { ...eventsFor, [name]: r.events }
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
      const [pods, status] = await Promise.all([
        api.deploymentPods(name),
        api.deploymentStatus(name).catch(() => null),
      ])
      expanded = { ...expanded, [name]: { pods: pods.pods, status } }
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

<Page title="Deployments" desc="Deploy an image as a k8s Deployment + Service">
  {#if error}<div class="mb-4 rounded-md border border-destructive/50 bg-destructive/10 p-3 text-sm text-destructive">{error}</div>{/if}

  <Card.Root class="mb-4">
    <Card.Header><Card.Title class="text-sm">Deploy image</Card.Title></Card.Header>
    <Card.Content class="space-y-3">
      <div class="grid grid-cols-2 gap-2">
        <Input.Root placeholder="name" bind:value={dName} />
        <Input.Root placeholder="image" bind:value={dImage} />
        <Input.Root type="number" bind:value={dReplicas} placeholder="replicas" />
        <Input.Root type="number" bind:value={dPort} placeholder="port (8080)" />
        <Input.Root class="col-span-2" placeholder="session (org:repo:bookmark, optional)" bind:value={dSession} />
        <div class="col-span-2 mt-1">
          <div class="mb-1 text-xs text-muted-foreground">Resources (optional, fallback to defaults)</div>
          <div class="grid grid-cols-4 gap-2">
            <Input.Root placeholder="cpu req" bind:value={dCpuRequest} />
            <Input.Root placeholder="mem req" bind:value={dMemRequest} />
            <Input.Root placeholder="cpu limit" bind:value={dCpuLimit} />
            <Input.Root placeholder="mem limit" bind:value={dMemLimit} />
          </div>
        </div>
      </div>
      <div class="flex flex-wrap items-center gap-2">
        <Button.Root onclick={deploy} disabled={deploying || !dName || !dImage}>
          <Rocket class="size-4" /> {deploying ? 'Deploying…' : 'Deploy'}
        </Button.Root>
        {#if dResult}<span class="text-xs text-muted-foreground">{dResult}</span>{/if}
      </div>
      {#if images.length}
        <details class="rounded-md border">
          <summary class="cursor-pointer px-3 py-2 text-xs text-muted-foreground">available images ({images.length}) — click to fill</summary>
          <div class="flex max-h-40 flex-wrap gap-1 overflow-auto border-t p-2">
            {#each images as img (img)}
              <button
                class="rounded bg-muted px-1.5 py-0.5 font-mono text-xs hover:bg-primary/20"
                onclick={() => (dImage = img)}
              >{img}</button
              >
            {/each}
          </div>
        </details>
      {/if}
    </Card.Content>
  </Card.Root>

  <Card.Root>
    <Card.Content class="pt-6">
      {#if deployments.length === 0}
        <p class="py-8 text-center text-sm text-muted-foreground">No deployments yet.</p>
      {:else}
        <div class="overflow-x-auto">
          <table class="w-full text-sm">
            <thead>
              <tr class="border-b text-left text-muted-foreground">
                <th class="py-2 pr-4 font-medium">Name</th>
                <th class="py-2 pr-4 font-medium">Image</th>
                <th class="py-2 pr-4 font-medium">Session</th>
                <th class="py-2 pr-4 font-medium">Replicas</th>
                <th class="py-2 pr-4 font-medium">Ports</th>
                <th class="py-2 pr-4 font-medium">Age</th>
                <th class="py-2 pr-4 font-medium"></th>
              </tr>
            </thead>
            <tbody>
              {#each deployments as d (d.name)}
                <tr class="border-b last:border-0">
                  <td class="py-2 pr-4">
                    <button class="flex items-center gap-1 font-mono text-xs hover:text-primary" onclick={() => toggle(d.name)}>
                      {#if expanded[d.name]}<ChevronDown class="size-3" />{:else}<ChevronRight class="size-3" />{/if}
                      {d.name}
                    </button>
                  </td>
                  <td class="py-2 pr-4 font-mono text-xs text-muted-foreground">{d.image}</td>
                  <td class="py-2 pr-4 font-mono text-xs text-muted-foreground">{d.session || '—'}</td>
                  <td class="py-2 pr-4">
                    <Badge variant={d.ready === d.replicas ? 'default' : 'secondary'}>
                      {d.ready}/{d.replicas}
                    </Badge>
                  </td>
                  <td class="py-2 pr-4 font-mono text-xs text-muted-foreground">{d.ports.join(', ')}</td>
                  <td class="py-2 pr-4 font-mono text-xs text-muted-foreground">{d.age}</td>
                  <td class="py-2 pr-4 text-right">
                    <div class="flex items-center justify-end gap-1">
                      <Button.Root
                        variant="ghost"
                        size="icon"
                        disabled={busy === d.name}
                        onclick={() => restart(d.name)}
                        title="Rolling restart"
                      >
                        <Rocket class="size-4" />
                      </Button.Root>
                      <Button.Root
                        variant="ghost"
                        size="icon"
                        disabled={busy === d.name}
                        onclick={() => scale(d.name, d.ready === d.replicas ? d.replicas + 1 : d.replicas)}
                        title="Scale up +1"
                      >
                        <span class="text-xs">+1</span>
                      </Button.Root>
                      <Button.Root
                        variant="ghost"
                        size="icon"
                        disabled={busy === d.name}
                        onclick={() => rollback(d.name)}
                        title="Rollback to previous revision"
                      >
                        <span class="text-xs">↩</span>
                      </Button.Root>
                      <Button.Root
                        variant="ghost"
                        size="icon"
                        disabled={busy === d.name}
                        onclick={() => showEvents(d.name)}
                        title="Show events"
                      >
                        <span class="text-xs">ev</span>
                      </Button.Root>
                      <Button.Root
                        variant="ghost"
                        size="icon"
                        disabled={busy === d.name}
                        onclick={() => remove(d.name)}
                        title="Delete deployment + service"
                      >
                        <Trash2 class="size-4 text-destructive" />
                      </Button.Root>
                    </div>
                  </td>
                </tr>
                {#if eventsFor[d.name]}
                  <tr>
                    <td colspan="7" class="bg-muted/30 px-4 py-2">
                      {#each eventsFor[d.name] as ev (ev.reason + ev.message + ev.age)}
                        <div class="flex items-start gap-2 py-0.5 font-mono text-[11px]">
                          <span class={ev.type === 'Warning' ? 'text-destructive' : 'text-muted-foreground'}>{ev.reason}</span>
                          <span class="flex-1 text-muted-foreground">{ev.message}</span>
                          <span class="text-muted-foreground">{ev.age}</span>
                        </div>
                      {/each}
                    </td>
                  </tr>
                {/if}
                {#if expanded[d.name]}
                  <tr>
                    <td colspan="7" class="bg-muted/30 px-4 py-3">
                      {#if expanded[d.name].status}
                        <div class="mb-2 flex flex-wrap gap-2 text-[11px] text-muted-foreground">
                          <span>updated={expanded[d.name].status!.updated_replicas}</span>
                          <span>ready={expanded[d.name].status!.ready_replicas}</span>
                          <span>available={expanded[d.name].status!.available_replicas}</span>
                        </div>
                      {/if}
                      {#each expanded[d.name].pods as p (p.name)}
                        <div class="flex items-center gap-2 py-0.5 font-mono text-[11px] text-muted-foreground">
                          <span>{p.name}</span>
                          <span>{p.ip || '—'}</span>
                          <span class={p.ready ? 'text-emerald-500' : 'text-destructive'}>{p.ready ? 'ready' : p.phase}</span>
                          <span>restarts={p.restarts}</span>
                          <span>{p.age}</span>
                        </div>
                      {/each}
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

