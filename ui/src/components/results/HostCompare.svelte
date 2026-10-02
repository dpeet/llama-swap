<script lang="ts">
  import { onMount } from "svelte";
  import { formatMeasurement, formatNumber, formatRatio, metricInfo, type ResultsData } from "../../lib/results";
  import {
    COMPARE_METRIC_LABELS,
    COMPARE_METRICS,
    comparePoints,
    emptyResultsFilters,
    parseYours,
    type CompareMetric,
    type ResultsFilters,
  } from "../../lib/resultsFilters";
  import { hostRows, plainText, rowRatio, shortConfigName, type HostRow, type HostValue } from "../../lib/resultsCompare";
  import { IsMobile } from "$lib/hooks/is-mobile.svelte.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import * as Select from "$lib/components/ui/select/index.js";
  import CompareStrip from "./CompareStrip.svelte";
  import Md from "./Md.svelte";

  interface Props {
    data: ResultsData;
    /** Only compareMetric, compareFamily and yours are read: the comparison doesn't follow the table's filters. */
    filters: ResultsFilters;
    families: string[];
    onchange: (patch: Partial<ResultsFilters>) => void;
    onselect: (configId: string) => void;
  }

  let { data, filters, families, onchange, onselect }: Props = $props();

  let family = $derived(filters.compareFamily);
  let metric = $derived(filters.compareMetric);
  let info = $derived(metricInfo(metric));
  let yours = $derived(parseYours(filters.yours));
  let invalid = $derived(filters.yours.trim() !== "" && yours === null);
  let rows = $derived(hostRows(data, family, metric));

  // The expanded view is today's dot strip, on the comparison's family and metric only.
  let stripFilters = $derived({ ...emptyResultsFilters(), family, compareMetric: metric, yours: filters.yours });
  let points = $derived(comparePoints(data, stripFilters));
  let showAll = $state(false);

  // Bars run from zero; the scale ends just past the largest of the bars, the best ticks and yours.
  let domainMax = $derived(
    Math.max(1e-9, yours ?? 0, ...rows.flatMap((r) => [r.value?.value ?? 0, r.best?.value ?? 0])) * 1.04,
  );
  const pct = (value: number) => `${Math.min(100, (value / domainMax) * 100)}%`;

  // A range-only record's mean is its midpoint; print that one number, not the range, because the bar is one value.
  const show = (v: HostValue) =>
    v.measurement.range ? formatNumber(v.value, v.measurement.precision ?? 1) : formatMeasurement(v.measurement, { aggregate: "mean" });

  // Ratio wording: for a time (TTFT) the ratio is yours ÷ theirs in time, so under 1× is faster.
  function answerItem(row: HostRow, r: number): string {
    return `${formatRatio(r)} ${row.label}`;
  }
  let answers = $derived(
    yours === null
      ? []
      : rows.flatMap((row) => {
          const r = rowRatio(row, yours);
          return r === null ? [] : [{ row, text: answerItem(row, r) }];
        }),
  );

  function accessibleName(row: HostRow): string {
    const parts = [row.label + (row.host ? ` (${row.host.label})` : "")];
    if (row.value) parts.push(`${show(row.value)} ${info.unit}`);
    else if (row.status === "no-value") parts.push("no value");
    else if (row.status === "no-profile") parts.push("no serving profile");
    if (row.config) parts.push(plainText(row.config.label));
    const r = rowRatio(row, yours);
    if (r !== null) parts.push(`yours is ${formatRatio(r)}`);
    if (row.best) parts.push(`best on this host ${show(row.best)} ${info.unit}, ${shortConfigName(row.best.config)}`);
    return parts.join(", ");
  }

  // RG bars use the strip's RG tokens (see CompareStrip RG_FILL), DGX production its primary fill.
  function barClass(row: HostRow): string {
    return row.key === "dgx-production" ? "bg-primary" : "bg-chart-3 dark:bg-chart-4";
  }

  // "Your number" gets focus on a desktop, not on a phone, where focus would pop the keyboard over the rows.
  const isMobile = new IsMobile();
  let yoursInput = $state<HTMLInputElement | null>(null);
  onMount(() => {
    if (!isMobile.current) yoursInput?.focus({ preventScroll: true });
  });
</script>

{#snippet rowBody(row: HostRow)}
  {@const r = rowRatio(row, yours)}
  {@const muted = row.status === "not-measured" || row.status === "no-profile"}
  <!-- host and config -->
  <div class="min-w-0 py-1.5 pr-3 text-left">
    <div class="truncate text-sm font-medium {muted ? 'text-muted-foreground' : ''}" title={row.host?.label}>{row.label}</div>
    <div class="text-muted-foreground truncate text-xs" title={row.config ? plainText(row.config.label) : undefined}>
      {#if row.config}<Md inline text={row.config.label} />{:else if row.status === "no-profile"}no serving profile{:else}not measured{/if}
    </div>
  </div>
  <!-- bar, best tick, yours line -->
  <div class="relative self-stretch">
    <div class="absolute inset-x-0 top-1/2 -translate-y-[calc(50%+7px)]">
      <div class="relative h-3.5">
        {#if row.value}
          <div class="absolute inset-y-0 left-0 rounded-r-sm {barClass(row)}" style:width={pct(row.value.value)}></div>
        {:else}
          <div class="border-muted-foreground/40 absolute inset-y-0 left-0 w-full border-b border-dashed" aria-hidden="true"></div>
        {/if}
        {#if row.best}
          <div class="bg-foreground absolute -top-1 -bottom-1 w-0.5 -translate-x-1/2" style:left={pct(row.best.value)}></div>
        {/if}
      </div>
    </div>
    {#if row.best}
      <div class="text-muted-foreground absolute inset-x-0 top-1/2 flex translate-y-[3px] items-center gap-1 text-[11px] leading-4">
        <span class="bg-foreground inline-block h-2.5 w-0.5 shrink-0" aria-hidden="true"></span>
        <span class="truncate">best {show(row.best)}<span class="hidden sm:inline">{` · ${shortConfigName(row.best.config)}`}</span></span>
      </div>
    {/if}
    {#if yours !== null}
      <div class="bg-foreground pointer-events-none absolute inset-y-0 w-[1.5px] -translate-x-1/2" style:left={pct(yours)}></div>
    {/if}
  </div>
  <!-- value and ratio -->
  <div class="py-1.5 pl-3 text-right">
    <div class="text-sm font-semibold tabular-nums {muted ? 'text-muted-foreground font-normal' : ''}">
      {#if row.value}{show(row.value)}<span class="text-muted-foreground ml-1 hidden text-xs font-normal sm:inline">{info.unit}</span>{:else if row.config}—{/if}
    </div>
    {#if r !== null}
      <div class="text-muted-foreground text-xs whitespace-nowrap tabular-nums">yours <span class="text-foreground font-semibold">{formatRatio(r)}</span></div>
    {/if}
  </div>
{/snippet}

<section class="rounded-lg border p-3 sm:p-4" aria-label="Compare a number with each host">
  <div class="flex flex-wrap items-end gap-3">
    <div class="space-y-1">
      <label class="text-sm font-medium" for="results-yours">Your number</label>
      <div class="flex items-center gap-1.5">
        <Input
          id="results-yours"
          bind:ref={yoursInput}
          class="h-10 w-36 text-lg md:text-lg"
          inputmode="decimal"
          autocomplete="off"
          placeholder="e.g. 150"
          value={filters.yours}
          oninput={(e) => onchange({ yours: e.currentTarget.value })}
          aria-invalid={invalid}
        />
        <span class="text-muted-foreground text-sm">{info.unit}</span>
      </div>
    </div>
    <div class="space-y-1">
      <label class="text-muted-foreground text-xs" for="results-compare-metric">Metric</label>
      <Select.Root type="single" value={metric} onValueChange={(v) => v && onchange({ compareMetric: v as CompareMetric })}>
        <Select.Trigger id="results-compare-metric" class="w-40 data-[size=default]:h-10">{COMPARE_METRIC_LABELS[metric]}</Select.Trigger>
        <Select.Content>
          {#each COMPARE_METRICS as m (m)}
            <Select.Item value={m}>{COMPARE_METRIC_LABELS[m]}</Select.Item>
          {/each}
        </Select.Content>
      </Select.Root>
    </div>
    <div class="space-y-1">
      <label class="text-muted-foreground text-xs" for="results-compare-family">Model family</label>
      <Select.Root type="single" value={family} onValueChange={(v) => v && onchange({ compareFamily: v })}>
        <Select.Trigger id="results-compare-family" class="w-32 data-[size=default]:h-10">{family}</Select.Trigger>
        <Select.Content>
          {#each families as f (f)}
            <Select.Item value={f}>{f}</Select.Item>
          {/each}
        </Select.Content>
      </Select.Root>
    </div>
  </div>

  <div class="mt-3 min-h-6 text-sm" aria-live="polite">
    {#if invalid}
      <span class="text-destructive">Type a positive number, like 150 or 1,200.</span>
    {:else if yours !== null && answers.length > 0}
      <!-- Gap, not separator glyphs, because Svelte trims a span's edge spaces and long items wrap. -->
      <p class="flex flex-wrap items-baseline gap-x-2 gap-y-0.5">
        <span class="font-semibold">{formatNumber(yours)} {info.unit}{info.higherIsBetter ? "" : ` ${COMPARE_METRIC_LABELS[metric]}`}:</span>
        {#each answers as a, i (a.row.key)}
          <span>{a.text}{info.higherIsBetter ? "" : "'s time"}{i < answers.length - 1 ? " ·" : ""}</span>
        {/each}
        {#if !info.higherIsBetter}<span class="text-muted-foreground">(lower is better: under 1× is faster)</span>{/if}
      </p>
    {:else if yours !== null}
      <span class="text-muted-foreground">No host has a {COMPARE_METRIC_LABELS[metric].toLowerCase()} value for {family}.</span>
    {:else}
      <span class="text-muted-foreground">Type a quoted number to compare it.</span>
    {/if}
  </div>

  <ul class="mt-2" aria-label="{COMPARE_METRIC_LABELS[metric]} by host, {family}">
    {#each rows as row (row.key)}
      {@const target = row.config ?? row.best?.config ?? null}
      <li class="border-border/60 border-b last:border-b-0">
        {#if target}
          <button
            type="button"
            class="hover:bg-muted/50 focus-visible:ring-ring grid min-h-11 w-full cursor-pointer grid-cols-[minmax(5.5rem,8rem)_1fr_5.5rem] items-center rounded-sm outline-none focus-visible:ring-2 focus-visible:ring-offset-2 focus-visible:ring-offset-background sm:grid-cols-[minmax(10rem,16rem)_1fr_7.5rem]"
            aria-label={accessibleName(row)}
            onclick={() => onselect(target.id)}
          >
            {@render rowBody(row)}
          </button>
        {:else}
          <div
            class="grid min-h-11 w-full grid-cols-[minmax(5.5rem,8rem)_1fr_5.5rem] items-center sm:grid-cols-[minmax(10rem,16rem)_1fr_7.5rem]"
            aria-label={accessibleName(row) + ", not measured"}
            role="group"
          >
            {@render rowBody(row)}
          </div>
        {/if}
      </li>
    {/each}
  </ul>

  <div class="text-muted-foreground mt-2 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs">
    <span>In {info.unit}, each host's serving config: mean of runs, fixed harness, thinking off.</span>
    <span class="inline-flex items-center gap-1.5"><span class="bg-foreground inline-block h-3 w-0.5" aria-hidden="true"></span>best measured on that host</span>
    {#if !info.higherIsBetter}<span>Lower is better.</span>{/if}
    <button
      type="button"
      class="text-foreground hover:bg-muted focus-visible:ring-ring ml-auto cursor-pointer rounded px-1.5 py-1 font-medium underline-offset-2 outline-none hover:underline focus-visible:ring-2"
      aria-expanded={showAll}
      aria-controls="results-all-configs"
      onclick={() => (showAll = !showAll)}
    >
      {showAll ? "Hide" : "Show"} all {points.length} configs
    </button>
  </div>

  {#if showAll}
    <div id="results-all-configs" class="mt-3 border-t pt-3">
      <CompareStrip {data} filters={stripFilters} {points} {families} {onchange} {onselect} embedded />
    </div>
  {/if}
</section>
