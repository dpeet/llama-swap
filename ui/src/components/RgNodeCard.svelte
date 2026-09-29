<script lang="ts">
  import { Info } from "@lucide/svelte";
  import * as Card from "$lib/components/ui/card/index.js";
  import * as Collapsible from "$lib/components/ui/collapsible/index.js";
  import * as Tooltip from "$lib/components/ui/tooltip/index.js";
  import { Badge } from "$lib/components/ui/badge/index.js";
  import { formatClock, holdStatus, speedCars } from "../lib/rg";
  import type { RgAvailability, RgFamily, RgHold, RgNode } from "../lib/types";

  interface Props {
    node: RgNode;
    /** DGX tok/s per family, the denominator for the cars. */
    reference: Record<RgFamily, number>;
    /** Our holds on this node. */
    holds: RgHold[];
  }

  let { node, reference, holds }: Props = $props();

  // The default profile sets the card's headline speed; the info box lists
  // every profile.
  let headline = $derived.by(() => {
    const profile = node.profiles.find((p) => p.default) ?? node.profiles[0];
    if (!profile) return null;
    return speedCars(profile.tok_s, reference[profile.family]);
  });

  // The variation selector keeps the car an emoji rather than a text glyph.
  const car = "\u{1F3CE}️";

  const badgeText: Record<RgAvailability, string> = {
    free: "Free",
    busy: "Busy",
    ours: "Yours",
    unavailable: "Unavailable",
    unknown: "Unknown",
  };
  const badgeVariant: Record<RgAvailability, "secondary" | "default" | "destructive" | "outline"> = {
    free: "secondary",
    busy: "outline",
    ours: "default",
    unavailable: "destructive",
    unknown: "outline",
  };

  function ratioText(ratio: number): string {
    return `≈${ratio >= 10 ? ratio.toFixed(0) : ratio.toFixed(1).replace(/\.0$/, "")}× DGX`;
  }

  let expanded = $state(false);
</script>

{#snippet details()}
  <dl class="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs">
    <dt class="opacity-70">Host</dt>
    <dd>{node.cpus} cores · {node.host_ram_gb} GB RAM · {node.arch}</dd>
    <dt class="opacity-70">Runtime</dt>
    <dd>{node.runtime}</dd>
    <dt class="opacity-70">Partition</dt>
    <dd>{node.partition} · MaxTime {node.max_time}</dd>
    {#if node.running}
      <dt class="opacity-70">Running</dt>
      <dd>{node.running.user} until {formatClock(node.running.end)}</dd>
    {/if}
    <dt class="opacity-70">Queue</dt>
    <dd>
      {#if node.queue.length === 0}
        nobody waiting
      {:else}
        {#each node.queue as entry (entry.job)}
          <div>{entry.user} · {entry.start_by ? `start by ${formatClock(entry.start_by)}` : "no start estimate"}</div>
        {/each}
      {/if}
    </dd>
    <dt class="opacity-70">Profiles</dt>
    <dd>
      {#if node.profiles.length === 0}
        none
      {:else}
        {#each node.profiles as profile (profile.id)}
          <div>
            {profile.label} · {profile.tok_s} tok/s ({ratioText(speedCars(profile.tok_s, reference[profile.family]).ratio)}) · {Math.round(profile.ctx_limit / 1000)}k ctx
          </div>
        {/each}
      {/if}
    </dd>
  </dl>
{/snippet}

<Card.Root size="sm">
  <Card.Content class="space-y-2">
    <div class="flex items-start justify-between gap-2">
      <div class="min-w-0">
        <div class="truncate font-semibold">{node.gpu}</div>
        <div class="text-muted-foreground truncate font-mono text-xs">{node.name}</div>
      </div>
      <Badge variant={badgeVariant[node.availability]}>{badgeText[node.availability]}</Badge>
    </div>

    <div class="flex flex-wrap items-baseline gap-x-3 gap-y-1">
      <span class="text-lg font-semibold">{node.memory_gb} GB</span>
      {#if headline}
        <span class="flex items-baseline gap-1.5" title="Default profile, relative to the DGX">
          <span role="img" aria-label={ratioText(headline.ratio)} class="tracking-tight">
            {car.repeat(headline.count)}
          </span>
          <span class="text-muted-foreground text-xs">{ratioText(headline.ratio)}</span>
        </span>
      {/if}
    </div>

    {#each holds as hold (hold.job)}
      <div class="text-xs">Your hold {hold.job}: {holdStatus(hold)}</div>
    {/each}

    <Collapsible.Root open={expanded}>
      <Tooltip.Root>
        <Tooltip.Trigger
          class="text-muted-foreground hover:text-foreground -ml-1 inline-flex items-center gap-1 rounded-md px-1 py-1 text-xs pointer-coarse:min-h-11"
          aria-expanded={expanded}
          aria-label="Details for {node.name}"
          onclick={() => (expanded = !expanded)}
        >
          <Info class="size-3.5" />
          Details
        </Tooltip.Trigger>
        {#if !expanded}
          <Tooltip.Content class="max-w-sm">{@render details()}</Tooltip.Content>
        {/if}
      </Tooltip.Root>
      <Collapsible.Content class="pt-1">
        {@render details()}
      </Collapsible.Content>
    </Collapsible.Root>
  </Card.Content>
</Card.Root>
