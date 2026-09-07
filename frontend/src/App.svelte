<script lang="ts">
  import { onMount } from 'svelte'
  import Overview from './pages/Overview.svelte'
  import Sandboxes from './pages/Sandboxes.svelte'
  import Sandbox from './pages/Sandbox.svelte'
  import Deployments from './pages/Deployments.svelte'
  import Helm from './pages/Helm.svelte'
  import Builds from './pages/Builds.svelte'
  import Packages from './pages/Packages.svelte'
  import Tools from './pages/Tools.svelte'
  import { Activity, Boxes, Terminal, Rocket, Hammer, Package, Wrench, Layers } from '@lucide/svelte'

  const links = [
    { href: '#/', label: 'Overview', icon: Activity, page: Overview },
    { href: '#/sandboxes', label: 'Sandboxes', icon: Boxes, page: Sandboxes },
    { href: '#/sandbox', label: 'Sandbox', icon: Terminal, page: Sandbox },
    { href: '#/deployments', label: 'Deployments', icon: Rocket, page: Deployments },
    { href: '#/helm', label: 'Helm', icon: Layers, page: Helm },
    { href: '#/builds', label: 'Builds', icon: Hammer, page: Builds },
    { href: '#/packages', label: 'Packages', icon: Package, page: Packages },
    { href: '#/tools', label: 'Tools', icon: Wrench, page: Tools },
  ]

  let hash = $state('#/')

  function sync() {
    hash = window.location.hash || '#/'
  }

  onMount(() => {
    sync()
    window.addEventListener('hashchange', sync)
    return () => window.removeEventListener('hashchange', sync)
  })

  const route = $derived(links.find((l) => l.href === hash) ?? links[0])
  const Current = $derived(route.page)
</script>

<div class="flex min-h-screen bg-background text-foreground">
  <nav class="flex w-52 shrink-0 flex-col border-r bg-card">
    <div class="flex items-center gap-2 border-b px-4 py-4">
      <div class="flex size-7 items-center justify-center rounded-md bg-primary font-bold text-primary-foreground text-xs">OP</div>
      <div class="leading-tight">
        <div class="text-sm font-semibold">ops-extension</div>
        <div class="text-[10px] text-muted-foreground">sandbox · builds · packages</div>
      </div>
    </div>
    {#each links as l (l.href)}
      <a
        href={l.href}
        class="flex items-center gap-2.5 border-l-2 px-4 py-2.5 text-sm transition-colors
          {l.href === route.href
            ? 'border-primary bg-primary/10 text-foreground'
            : 'border-transparent text-muted-foreground hover:bg-muted hover:text-foreground'}"
      >
        <l.icon class="size-4" />
        {l.label}
      </a>
    {/each}
  </nav>
  <main class="min-w-0 flex-1">
    <Current />
  </main>
</div>
