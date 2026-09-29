<script lang="ts">
  import { Info } from "@lucide/svelte";
  import * as Card from "$lib/components/ui/card/index.js";
  import * as Collapsible from "$lib/components/ui/collapsible/index.js";
  import * as Tooltip from "$lib/components/ui/tooltip/index.js";
  import { Badge } from "$lib/components/ui/badge/index.js";
  import { advanceIso, busyBadgeLabel, busyTarget, busyTone, formatClock, holdStatus, nodeDisplayState, nodePopoverDetails, queuedHoldLine, speedCars, type BusyTone, type NodeDisplayState } from "../lib/rg";
  import type { RgFamily, RgHold, RgNode } from "../lib/types";

  interface Props {
    node: RgNode;
    /** DGX tok/s per family, the denominator for the cars. */
    reference: Record<RgFamily, number>;
    /** Our holds on this node. */
    holds: RgHold[];
    /** The overview's `generated_at`, the "now" for the busy-for estimate. */
    generatedAt?: string;
    /** Seconds since the overview was fetched; advances every time-left, no request. */
    elapsedS?: number;
  }

  let { node, reference, holds, generatedAt, elapsedS = 0 }: Props = $props();

  // The snapshot time advanced to now, so the busy label and tone count down.
  let busyNow = $derived(advanceIso(generatedAt, elapsedS));

  // The default profile sets the card's headline speed; the info box lists
  // every profile.
  let headline = $derived.by(() => {
    const profile = node.profiles.find((p) => p.default) ?? node.profiles[0];
    if (!profile) return null;
    return speedCars(profile.tok_s, reference[profile.family]);
  });

  // The variation selector keeps the car an emoji rather than a text glyph.
  const car = "\u{1F3CE}️";

  // "queued" and "yours" both come from availability "ours"; only our hold
  // running makes the node ours, a pending one leaves someone else on it.
  let displayState = $derived(nodeDisplayState(node.availability, holds));

  const badgeText: Record<NodeDisplayState, string> = {
    free: "Free",
    busy: "Busy",
    queued: "Queued",
    ours: "Yours",
    yours: "Yours",
    unavailable: "Unavailable",
    unknown: "Unknown",
  };
  const badgeVariant: Record<NodeDisplayState, "secondary" | "default" | "destructive" | "outline"> = {
    free: "secondary",
    busy: "outline",
    queued: "outline",
    ours: "default",
    yours: "default",
    unavailable: "destructive",
    unknown: "outline",
  };

  // A busy node says how long. A queued one shows "Queued" plus the same label
  // for its current occupant, so the two waits compare at a glance; it has no
  // occupant label when the overview has no `running`. A missing end or
  // snapshot time leaves the plain "Busy".
  let hasOccupant = $derived(displayState === "busy" || (displayState === "queued" && node.running != null));
  let target = $derived(busyTarget(node, displayState));
  let occupantLabel = $derived(busyBadgeLabel(target.iso, busyNow, target.incomplete));

  // Tinted like the Release button (destructive/10 light, /20 dark), but with
  // a deeper light-mode and paler dark-mode text than `text-destructive`,
  // because that measures APCA Lc 58 / -42 on its own tint, under the 60 floor
  // for labels. Measured on a default-theme card: red 67/-77, orange 67/-67,
  // yellow 69/-81 (light/dark). Queued sky uses the same 700 / 300 steps but
  // was not measured.
  const toneClass: Record<BusyTone, string> = {
    red: "border-transparent bg-destructive/10 text-red-700 dark:bg-destructive/20 dark:text-red-200",
    orange: "border-transparent bg-orange-500/10 text-orange-700 dark:bg-orange-500/20 dark:text-orange-300",
    yellow: "border-transparent bg-yellow-500/10 text-yellow-700 dark:bg-yellow-500/20 dark:text-yellow-300",
  };
  // Blue, so "Queued" is neither the teal "Yours" nor one of the busy tones.
  const queuedClass = "border-transparent bg-sky-500/10 text-sky-700 dark:bg-sky-500/20 dark:text-sky-300";
  // Null (unknown end) keeps the neutral outline badge.
  let occupantTone = $derived(hasOccupant ? busyTone(target.iso, busyNow) : null);

  function ratioText(ratio: number): string {
    return `≈${ratio >= 10 ? ratio.toFixed(0) : ratio.toFixed(1).replace(/\.0$/, "")}× DGX`;
  }

  let expanded = $state(false);

  // The busy badge is a button that opens these rows inline, because a tooltip
  // never opens on a tap; the hover tooltip stays as the desktop shortcut.
  let busyOpen = $state(false);
  // On our own node the same rows answer "is anyone queued behind me?"
  let ourJobs = $derived(new Set(holds.map((hold) => hold.job)));
  let busyRows = $derived(nodePopoverDetails(node, busyNow, undefined, ourJobs));
  // The badge that opens busyRows: the occupant's time for busy/queued, "Yours" on our node.
  let detailBadge = $derived(
    displayState === "busy" || hasOccupant
      ? { text: occupantLabel, variant: "outline" as const, cls: occupantTone ? toneClass[occupantTone] : undefined }
      : displayState === "yours"
        ? { text: badgeText.yours, variant: badgeVariant.yours, cls: undefined }
        : null,
  );
</script>

{#snippet busyDetails()}
  <div class="flex flex-col gap-1 text-xs">
    {#each busyRows as row}
      <div>{row}</div>
    {/each}
  </div>
{/snippet}

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
      <div class="flex shrink-0 flex-wrap justify-end gap-1">
        {#if displayState === "queued"}
          <Badge variant="outline" class={queuedClass}>{badgeText.queued}</Badge>
        {/if}
        {#if detailBadge}
          {#if busyRows.length > 0}
            <Tooltip.Root delayDuration={150}>
              <Tooltip.Trigger
                class="focus-visible:ring-ring/50 rounded-none text-left focus-visible:ring-[3px] focus-visible:outline-none pointer-coarse:min-h-11 pointer-coarse:flex pointer-coarse:items-center"
                aria-expanded={busyOpen}
                aria-controls="busy-details-{node.name}"
                aria-label="{detailBadge.text}, details for {node.name}"
                onclick={() => (busyOpen = !busyOpen)}
              >
                <Badge variant={detailBadge.variant} class={detailBadge.cls}>{detailBadge.text}</Badge>
              </Tooltip.Trigger>
              {#if !busyOpen}
                <Tooltip.Content class="max-w-sm">{@render busyDetails()}</Tooltip.Content>
              {/if}
            </Tooltip.Root>
          {:else}
            <Badge variant={detailBadge.variant} class={detailBadge.cls}>{detailBadge.text}</Badge>
          {/if}
        {:else if displayState !== "queued"}
          <Badge variant={badgeVariant[displayState]}>{badgeText[displayState]}</Badge>
        {/if}
      </div>
    </div>

    {#if busyOpen && busyRows.length > 0}
      <div id="busy-details-{node.name}" class="bg-muted/50 p-2">
        {@render busyDetails()}
      </div>
    {/if}

    <div class="flex flex-wrap items-baseline gap-x-3 gap-y-1">
      <span class="text-lg font-semibold">{node.memory_gb} GB</span>
      {#if headline}
        <span class="flex flex-wrap items-baseline gap-x-2" title="Default profile, relative to the DGX">
          <!-- text-xl plus tracking-wider, because the old tracking-tight overlapped the red
               cars into an unreadable smudge on desktop dark mode; wraps below the text on a narrow card. -->
          <span role="img" aria-label={ratioText(headline.ratio)} class="text-xl leading-none tracking-wider whitespace-nowrap">
            {car.repeat(headline.count)}
          </span>
          <span class="text-muted-foreground text-xs">{ratioText(headline.ratio)}</span>
        </span>
      {/if}
    </div>

    {#each holds as hold (hold.job)}
      <div class="text-xs">
        Your hold {hold.job}: {hold.state === "PENDING" ? queuedHoldLine(hold, busyNow) : holdStatus(hold, elapsedS)}
      </div>
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
