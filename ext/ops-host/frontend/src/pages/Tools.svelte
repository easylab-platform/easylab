<script lang="ts">
  import Page from '$lib/components/Page.svelte'
  import * as Card from '$lib/components/ui/card'
  import * as Badge from '$lib/components/ui/badge'

  const groups: { group: string; tools: { name: string; desc: string }[] }[] = [
    {
      group: 'Sandbox (session-scoped, auto-synced)',
      tools: [
        ['sandbox-run', 'Run a shell command; workspace synced to the repo bookmark head first'],
        ['sandbox-read', 'Read a file (binary-safe)'],
        ['sandbox-write', 'Write a file (no sync first — never clobbers in-progress edits)'],
        ['sandbox-edit', 'Replace/insert lines by line numbers'],
        ['sandbox-port', 'Copy a sandbox file into the session repo (bookmark moves)'],
      ],
    },
    {
      group: 'Sandbox jobs',
      tools: [
        ['sandbox-job-list', 'List jobs'],
        ['sandbox-job-output', 'Read job output (offset/limit/grep)'],
        ['sandbox-job-wait', 'Wait for a job'],
        ['sandbox-job-stdin', 'Send stdin to a job'],
        ['sandbox-job-kill', 'Kill a job'],
      ],
    },
    {
      group: 'Images',
      tools: [
        ['container-build', 'Build a Containerfile from the repo via buildkitd + push'],
        ['service-deploy', 'Deploy an image as a k8s Deployment (nested resources)'],
        ['image-list', 'List OCI registry images'],
        ['list-containerfile-templates', 'Built-in Containerfile templates'],
      ],
    },
    {
      group: 'Helm',
      tools: [
        ['helm-install', 'Install/upgrade a Helm chart from the repo checkout (chart_path)'],
        ['helm-list', 'List Helm releases in the namespace'],
        ['helm-status', 'Show one release status'],
        ['helm-uninstall', 'Uninstall a release'],
      ],
    },
    {
      group: 'Packages',
      tools: [
        ['package-publish', 'Publish the repo checkout as a package (14 protocols, official CLIs)'],
        ['list-registry-packages', 'List packages across protocols'],
      ],
    },
    {
      group: 'Repos',
      tools: [['pull-git-repo', "Clone an external git repo into jjlab (org 'external')"]],
    },
  ]
</script>

<Page title="Tools" desc="21 NATS tools exposed to the agent (discovered via the extension SDK)">
  <div class="space-y-4">
    {#each groups as g (g.group)}
      <Card.Root>
        <Card.Header><Card.Title class="text-sm">{g.group}</Card.Title></Card.Header>
        <Card.Content class="space-y-2">
          {#each g.tools as t (t[0])}
            <div class="flex flex-col gap-0.5 border-b py-2 last:border-0 sm:flex-row sm:items-baseline sm:gap-3">
              <code class="shrink-0 font-mono text-xs text-primary">{t[0]}</code>
              <span class="text-xs text-muted-foreground">{t[1]}</span>
            </div>
          {/each}
        </Card.Content>
      </Card.Root>
    {/each}
  </div>
</Page>
