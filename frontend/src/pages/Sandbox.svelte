<script lang="ts">
  import { onMount, onDestroy } from 'svelte'
  import { api, sandboxJobStreamUrl, type Sandbox, type Job } from '$lib/api'
  import Page from '$lib/components/Page.svelte'
  import * as Card from '$lib/components/ui/card'
  import * as Button from '$lib/components/ui/button'
  import * as Input from '$lib/components/ui/input'
  import * as Select from '$lib/components/ui/native-select'
  import { Play, FileText, Save, Ban, RefreshCw } from '@lucide/svelte'

  let sandboxes: Sandbox[] = $state([])
  let session = $state('')
  let cmd = $state('')
  let output = $state('')
  let running = $state(false)

  let filePath = $state('')
  let fileContent = $state('')
  let fileSaved = $state('')

  let jobs: Job[] = $state([])
  let termJobId = $state('')
  let termHistory = $state('')
  let termDone = $state(false)
  let jobEs: EventSource | null = null

  async function refreshSandboxes() {
    try {
      sandboxes = (await api.sandboxes()).sandboxes
      if (!session && sandboxes.length) session = sandboxes[0].session
    } catch {}
  }

  async function run() {
    if (!cmd.trim()) return
    running = true
    output = ''
    try {
      const r = await api.exec(session, cmd)
      // Every command is a streamed job (no fast/slow split).
      if (r.job_id) {
        output = `[job: ${r.job_id}]`
        openJobStream(r.job_id)
        await refreshJobs()
      } else {
        output = `exit=${r.exit_code}\n` + (r.output ?? '')
      }
    } catch (e) {
      output = String(e)
    } finally {
      running = false
    }
  }

  function openJobStream(jobId: string) {
    closeJobEs()
    termJobId = jobId
    termHistory = ''
    termDone = false
    jobEs = new EventSource(sandboxJobStreamUrl(session, jobId))
    jobEs.addEventListener('job.output', (e) => {
      try {
        const p = JSON.parse((e as MessageEvent).data)
        if (p.job_id !== jobId) return
        termHistory += p.content ?? ''
      } catch {}
    })
    jobEs.addEventListener('job.history_end', (e) => {
      try {
        const p = JSON.parse((e as MessageEvent).data)
        termHistory += `\n[history_end: total ${p.total ?? 0} lines]\n`
      } catch {}
    })
    jobEs.addEventListener('job.completed', (e) => {
      try {
        const p = JSON.parse((e as MessageEvent).data)
        if (p.job_id !== jobId) return
        termDone = true
        termHistory += `\n[${p.state ?? 'done'} exit=${p.exit_code}]\n`
        refreshJobs()
      } catch {}
    })
    jobEs.onerror = () => {
      if (!termDone) termHistory += '\n[stream closed]'
    }
  }

  function closeJobEs() {
    if (jobEs) {
      jobEs.close()
      jobEs = null
    }
  }

  async function openFile() {
    fileSaved = ''
    try {
      const r = await api.readFile(session, filePath)
      fileContent = r.content ?? r.error ?? ''
    } catch (e) {
      fileContent = ''
      fileSaved = String(e)
    }
  }

  async function saveFile() {
    try {
      const r = await api.writeFile(session, filePath, fileContent)
      fileSaved = r.ok ? 'saved' : (r.error ?? 'failed')
    } catch (e) {
      fileSaved = String(e)
    }
  }

  async function refreshJobs() {
    try {
      jobs = (await api.jobs(session)).jobs.jobs
    } catch {
      jobs = []
    }
  }

  async function kill(id: string) {
    await api.jobKill(session, id)
    await refreshJobs()
  }

  onMount(() => {
    refreshSandboxes()
    const t = setInterval(refreshSandboxes, 10_000)
    return () => clearInterval(t)
  })

  onDestroy(() => closeJobEs())

  $effect(() => {
    void session
    refreshJobs()
  })
</script>

<Page title="Sandbox" desc="Exec console, file editor and jobs for a session's worker pod">
  {#if !sandboxes.length}
    <Card.Root><Card.Content class="pt-6 text-sm text-muted-foreground">No running sandboxes.</Card.Content></Card.Root>
  {:else}
    <div class="mb-4 flex items-center gap-3">
      <Select.Root bind:value={session}>
        {#each sandboxes as s (s.container_id)}
          <option value={s.session}>{s.session || s.pod_name}</option>
        {/each}
      </Select.Root>
    </div>

    <div class="grid gap-4 lg:grid-cols-2">
      <Card.Root>
        <Card.Header><Card.Title class="text-sm">Exec</Card.Title></Card.Header>
        <Card.Content class="space-y-3">
          <div class="flex gap-2">
            <Input.Root placeholder="echo hello" bind:value={cmd} onkeydown={(e) => e.key === 'Enter' && run()} />
            <Button.Root size="sm" onclick={run} disabled={running || !cmd.trim()}>
              <Play class="size-4" /> Run
            </Button.Root>
          </div>
          <pre class="max-h-64 overflow-auto rounded-md bg-muted p-3 font-mono text-xs whitespace-pre-wrap">{output || '—'}</pre>
        </Card.Content>
      </Card.Root>

      {#if termJobId}
        <Card.Root>
          <Card.Header class="flex-row items-center justify-between">
            <Card.Title class="text-sm">Job stream: {termJobId}</Card.Title>
            <span class="text-xs {termDone ? 'text-emerald-500' : 'text-amber-500'}">{termDone ? 'done' : 'streaming…'}</span>
          </Card.Header>
          <Card.Content>
            <pre class="max-h-64 overflow-auto rounded-md bg-muted p-3 font-mono text-xs whitespace-pre-wrap">{termHistory || 'waiting for output…'}</pre>
          </Card.Content>
        </Card.Root>
      {/if}

      <Card.Root>
        <Card.Header><Card.Title class="text-sm">Files</Card.Title></Card.Header>
        <Card.Content class="space-y-3">
          <div class="flex gap-2">
            <Input.Root placeholder="src/main.rs" bind:value={filePath} onkeydown={(e) => e.key === 'Enter' && openFile()} />
            <Button.Root size="sm" variant="outline" onclick={openFile}><FileText class="size-4" /> Open</Button.Root>
            <Button.Root size="sm" onclick={saveFile} disabled={!filePath}><Save class="size-4" /> Save</Button.Root>
          </div>
          <textarea
            class="h-48 w-full rounded-md bg-muted p-3 font-mono text-xs"
            placeholder="file content"
            bind:value={fileContent}
          ></textarea>
          {#if fileSaved}<p class="text-xs text-muted-foreground">{fileSaved}</p>{/if}
        </Card.Content>
      </Card.Root>
    </div>

    <Card.Root class="mt-4">
      <Card.Header class="flex-row items-center justify-between">
        <Card.Title class="text-sm">Jobs</Card.Title>
        <Button.Root variant="ghost" size="icon" onclick={refreshJobs}><RefreshCw class="size-4" /></Button.Root>
      </Card.Header>
      <Card.Content>
        {#if jobs.length === 0}
          <p class="py-4 text-center text-sm text-muted-foreground">No jobs.</p>
        {:else}
          <table class="w-full text-sm">
            <thead>
              <tr class="border-b text-left text-muted-foreground">
                <th class="py-2 pr-4 font-medium">ID</th>
                <th class="py-2 pr-4 font-medium">Command</th>
                <th class="py-2 pr-4 font-medium">State</th>
                <th class="py-2 pr-4 font-medium">Exit</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {#each jobs as j (j.id)}
                <tr class="border-b last:border-0">
                  <td class="py-2 pr-4 font-mono text-xs">{j.id.slice(0, 10)}</td>
                  <td class="py-2 pr-4 font-mono text-xs">{j.command.slice(0, 60)}</td>
                  <td class="py-2 pr-4">{j.state}</td>
                  <td class="py-2 pr-4">{j.exit_code}</td>
                  <td class="text-right">
                    <Button.Root variant="ghost" size="icon" onclick={() => kill(j.id)}><Ban class="size-4 text-destructive" /></Button.Root>
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        {/if}
      </Card.Content>
    </Card.Root>
  {/if}
</Page>
