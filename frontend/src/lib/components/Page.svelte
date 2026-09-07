<script lang="ts">
  import type { Snippet } from 'svelte'

  let {
    title,
    desc = '',
    right = null,
    children,
  }: {
    title: string
    desc?: string
    right?: Snippet
    children: Snippet
  } = $props()
</script>

<div class="mx-auto max-w-6xl px-6 py-8">
  <div class="mb-6 flex items-end justify-between gap-4">
    <div>
      <h1 class="text-2xl font-semibold tracking-tight text-foreground">{title}</h1>
      {#if desc}<p class="mt-1 text-sm text-muted-foreground">{desc}</p>{/if}
    </div>
    {#if right}{@render right()}{/if}
  </div>
  {@render children()}
</div>
