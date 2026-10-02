<script lang="ts">
  import { formatMeasurement, formatNumber, formatRatio, metricInfo, type ResultsData } from "../../lib/results";
  import {
    COMPARE_METRIC_LABELS,
    COMPARE_METRICS,
    compareRatios,
    parseYours,
    type CompareMetric,
    type ComparePoint,
    type CompareRatio,
    type ResultsFilters,
  } from "../../lib/resultsFilters";
  import { niceMax, plainText } from "../../lib/resultsCompare";
  import { Input } from "$lib/components/ui/input/index.js";
  import * as Select from "$lib/components/ui/select/index.js";

  interface Props {
    data: ResultsData;
    filters: ResultsFilters;
    points: ComparePoint[];
    families: string[];
    onchange: (patch: Partial<ResultsFilters>) => void;
    onselect: (configId: string) => void;
    /** Under HostCompare's "show all configs": no controls or ratio line of its own, because the rows above carry them. */
    embedded?: boolean;
    /** The x-scale end, when a parent shares one with its own marks (HostCompare's rows). */
    domainMax?: number;
    /** A grid wrapper and the cell the plot sits in, so the plot spans the same x range as the parent's bar track. */
    plotGrid?: string;
    plotCell?: string;
  }

  let {
    data,
    filters,
    points,
    families,
    onchange,
    onselect,
    embedded = false,
    domainMax: sharedDomainMax,
    plotGrid = "",
    plotCell = "",
  }: Props = $props();

  const ALL = "__all__";
  let info = $derived(metricInfo(filters.compareMetric));
  let yours = $derived(parseYours(filters.yours, info.unit));
  let ratios = $derived(compareRatios(points, yours));
  let productionCount = $derived(points.filter((p) => p.dgxProduction).length);
  let nearestRg = $derived(ratios.find((r) => r.kind === "nearest-rg")?.point ?? null);
  // The legend names only what is plotted.
  let plotted = $derived({
    production: points.some((p) => p.dgxProduction),
    otherDgx: points.some((p) => p.site !== "rg" && !p.dgxProduction),
    rg: points.some((p) => p.site === "rg"),
  });

  // RG marks use chart tokens, not a palette hue, because amber equalled
  // --primary (the DGX production fill) under the Amber theme. chart-3 (navy)
  // light and chart-4 (purple) dark, because no theme's primary is either; the
  // nearest is violet-dark's, where square-vs-circle still tells them apart.
  const RG_FILL = "fill-chart-3 dark:fill-chart-4";
  const RG_SWATCH = "bg-chart-3 dark:bg-chart-4";

  function ratioName(r: CompareRatio): string {
    const config = r.point.config;
    if (r.kind === "dgx-production") {
      // Two production lanes (Flash-Next and 27B) show when no family is picked, so name the model.
      return productionCount > 1 ? `DGX production ${config.model}` : "DGX production";
    }
    const gpu = data.hosts[config.host]?.gpu ?? config.host;
    return `${gpu} ${config.matrix_id ?? config.label}`;
  }

  // ---- Geometry: plain SVG in CSS pixels (bind:clientWidth), so labels stay
  // legible at 390 px instead of shrinking with a fixed viewBox.
  let width = $state(640);
  // Embedded, the plot spans the parent's bar track edge to edge, so x matches the bars.
  let PAD_X = $derived(embedded ? 0 : 14);
  const DOT_R = 5;
  const PROD_R = 7;
  const ROW_STEP = 12;
  const LABEL_LINE = 12;
  const AXIS_GAP = 10;
  const AXIS_H = 22;

  let domainMax = $derived(sharedDomainMax ?? niceMax(Math.max(...points.map((p) => p.value), yours ?? 0) * 1.04));
  let plotW = $derived(Math.max(width - PAD_X * 2, 100));
  const x = (value: number) => PAD_X + (value / domainMax) * plotW;

  // A beeswarm-lite: each dot takes the lowest row where it doesn't overlap the
  // previous dot on that row, so near-equal configs stack instead of hiding.
  let placed = $derived.by(() => {
    const rowEnds: number[] = [];
    return points.map((point) => {
      const px = x(point.value);
      const r = point.dgxProduction ? PROD_R : DOT_R;
      let row = rowEnds.findIndex((end) => px - end >= 2 * DOT_R + 2);
      if (row === -1) row = rowEnds.length;
      rowEnds[row] = px + (r - DOT_R);
      return { point, px, row, r };
    });
  });
  let rows = $derived(Math.max(1, ...placed.map((p) => p.row + 1)));

  // Value labels for the highlighted dots sit above the tallest stack under the label's own width, so no dot
  // covers them; a label that would overlap an earlier one moves up a line at a time (its lane).
  const labelText = (p: ComparePoint) => formatMeasurement(p.measurement, { aggregate: "mean" });
  const labelWidth = (text: string) => text.length * 6.5 + 4; // 11px tabular digits run ~6.3px each
  let labels = $derived.by(() => {
    const out: { point: ComparePoint; px: number; text: string; lane: number; rise: number; left: number; right: number }[] = [];
    for (const { point, px } of placed) {
      if (!(point.dgxProduction || point === nearestRg)) continue;
      const text = labelText(point);
      const w = labelWidth(text);
      const anchor = anchorFor(px);
      const left = anchor === "start" ? px : anchor === "end" ? px - w : px - w / 2;
      const right = left + w;
      const under = placed.filter((q) => q.px + q.r >= left && q.px - q.r <= right);
      // how far above the baseline the tallest mark under the label reaches
      const rise = Math.max(0, ...under.map((q) => q.row * ROW_STEP + DOT_R + q.r));
      let lane = 0;
      const lift = (o: { rise: number; lane: number }) => o.rise + o.lane * LABEL_LINE;
      while (out.some((o) => o.left < right && left < o.right && Math.abs(lift(o) - lift({ rise, lane })) < LABEL_LINE)) lane++;
      out.push({ point, px, text, lane, rise, left, right });
    }
    return out;
  });
  // Room above the dots for the "yours" label and every label; 34 fits one label line over the tallest stack.
  let TOP = $derived(34 + Math.max(0, ...labels.map((l) => l.rise + l.lane * LABEL_LINE - rows * ROW_STEP)));
  let baseY = $derived(TOP + rows * ROW_STEP);
  let axisY = $derived(baseY + AXIS_GAP);
  let height = $derived(axisY + AXIS_H);
  let ticks = $derived([0, domainMax / 2, domainMax]);

  function dotY(row: number): number {
    return baseY - row * ROW_STEP - DOT_R;
  }

  function anchorFor(px: number): "start" | "middle" | "end" {
    if (px < 60) return "start";
    if (px > width - 60) return "end";
    return "middle";
  }

  function pointTitle(p: ComparePoint): string {
    const host = data.hosts[p.config.host]?.label ?? p.config.host;
    return `${plainText(p.config.label)} · ${host}\n${formatMeasurement(p.measurement, { aggregate: "mean" })} ${info.unit} (mean of ${formatMeasurement(p.measurement)})`;
  }

  function onDotKey(event: KeyboardEvent, id: string): void {
    if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      onselect(id);
    }
  }
</script>

<section class={embedded ? "" : "rounded-lg border p-3 sm:p-4"} aria-label={embedded ? "Every config" : "Compare a number"}>
  {#if !embedded}
  <div class="flex flex-wrap items-end gap-3">
    <div class="space-y-1">
      <label class="text-muted-foreground text-xs" for="results-compare-metric">Metric</label>
      <Select.Root
        type="single"
        value={filters.compareMetric}
        onValueChange={(v) => v && onchange({ compareMetric: v as CompareMetric })}
      >
        <Select.Trigger id="results-compare-metric" class="w-44">{COMPARE_METRIC_LABELS[filters.compareMetric]}</Select.Trigger>
        <Select.Content>
          {#each COMPARE_METRICS as metric (metric)}
            <Select.Item value={metric}>{COMPARE_METRIC_LABELS[metric]}</Select.Item>
          {/each}
        </Select.Content>
      </Select.Root>
    </div>
    <div class="space-y-1">
      <label class="text-muted-foreground text-xs" for="results-compare-family">Model family</label>
      <Select.Root
        type="single"
        value={filters.family === "" ? ALL : filters.family}
        onValueChange={(v) => v && onchange({ family: v === ALL ? "" : v })}
      >
        <Select.Trigger id="results-compare-family" class="w-36">{filters.family === "" ? "All families" : filters.family}</Select.Trigger>
        <Select.Content>
          <Select.Item value={ALL}>All families</Select.Item>
          {#each families as family (family)}
            <Select.Item value={family}>{family}</Select.Item>
          {/each}
        </Select.Content>
      </Select.Root>
    </div>
    <div class="space-y-1">
      <label class="text-muted-foreground text-xs" for="results-yours">Your number</label>
      <div class="flex items-center gap-1.5">
        <Input
          id="results-yours"
          class="w-28"
          inputmode="decimal"
          autocomplete="off"
          placeholder="e.g. 150"
          value={filters.yours}
          oninput={(e) => onchange({ yours: e.currentTarget.value })}
          aria-invalid={filters.yours.trim() !== "" && yours === null}
        />
        <span class="text-muted-foreground text-xs">{info.unit}</span>
      </div>
    </div>
  </div>

  <div class="mt-3 min-h-6 text-sm" aria-live="polite">
    {#if filters.yours.trim() !== "" && yours === null}
      <span class="text-destructive">Type a positive number, like 150 or 1,200.</span>
    {:else if yours !== null && ratios.length > 0}
      <!-- Gap, not separator glyphs, because Svelte trims a span's edge spaces and long chips wrap. -->
      <div class="flex flex-wrap items-baseline gap-x-3 gap-y-1">
        <span class="text-muted-foreground">{formatNumber(yours)} {info.unit} is</span>
        {#each ratios as r (r.point.config.id + r.kind)}
          <button
            type="button"
            class="hover:bg-muted cursor-pointer rounded px-0.5 text-left font-semibold"
            onclick={() => onselect(r.point.config.id)}
            title="Open {r.point.config.label}"
          >
            {formatRatio(r.ratio)} {ratioName(r)}
          </button>
        {/each}
        {#if !info.higherIsBetter}<span class="text-muted-foreground">(lower is better)</span>{/if}
      </div>
    {:else if yours !== null}
      <span class="text-muted-foreground">No DGX production or RG config has this metric under these filters.</span>
    {:else}
      <span class="text-muted-foreground">Type a number someone quoted to see it against DGX production and the nearest RG config.</span>
    {/if}
  </div>
  {/if}

  <div class="mt-2 {plotGrid}"><div class="w-full min-w-0 {plotCell}" bind:clientWidth={width}>
    {#if points.length === 0}
      <p class="text-muted-foreground py-6 text-center text-sm">
        No config has a {COMPARE_METRIC_LABELS[filters.compareMetric].toLowerCase()} measurement under these filters.
      </p>
    {:else}
      <svg {width} {height} viewBox="0 0 {width} {height}" class="block overflow-visible" role="img" aria-label="{info.label} by config">
        <!-- axis -->
        <line x1={PAD_X} y1={axisY} x2={PAD_X + plotW} y2={axisY} stroke="currentColor" opacity="0.25" />
        {#each ticks as tick, i (i)}
          <line x1={x(tick)} y1={axisY} x2={x(tick)} y2={axisY + 4} stroke="currentColor" opacity="0.4" />
          <text
            x={x(tick)}
            y={axisY + 16}
            font-size="11"
            fill="currentColor"
            opacity="0.7"
            text-anchor={i === 0 ? "start" : i === ticks.length - 1 ? "end" : "middle"}
          >
            {formatNumber(tick, tick >= 10 || tick === 0 ? 0 : tick >= 1 ? 1 : 2)}{i === ticks.length - 1 ? ` ${info.unit}` : ""}
          </text>
        {/each}

        <!-- the typed number -->
        {#if yours !== null}
          <line x1={x(yours)} y1={14} x2={x(yours)} y2={axisY} stroke="currentColor" stroke-width="1.5" class="text-foreground" />
          <text x={x(yours)} y={11} font-size="11" font-weight="600" fill="currentColor" text-anchor={anchorFor(x(yours))}>
            yours {formatNumber(yours)}
          </text>
        {/if}

        {#each placed as { point, px, row, r } (point.config.id)}
          {@const cy = dotY(row)}
          <g
            role="button"
            tabindex="0"
            aria-label="{plainText(point.config.label)}, {formatMeasurement(point.measurement, { aggregate: 'mean' })} {info.unit}"
            class="group cursor-pointer outline-none"
            onclick={() => onselect(point.config.id)}
            onkeydown={(e) => onDotKey(e, point.config.id)}
          >
            <title>{pointTitle(point)}</title>
            <!-- keyboard focus ring, 3 px outside the mark in its own shape -->
            {#if point.site === "rg"}
              <rect
                x={px - DOT_R - 3}
                y={cy - DOT_R - 3}
                width={(DOT_R + 3) * 2}
                height={(DOT_R + 3) * 2}
                fill="none"
                stroke="var(--ring)"
                stroke-width="2"
                class="opacity-0 group-focus-visible:opacity-100"
              />
            {:else}
              <circle cx={px} {cy} r={r + 3} fill="none" stroke="var(--ring)" stroke-width="2" class="opacity-0 group-focus-visible:opacity-100" />
            {/if}
            {#if point.site === "rg"}
              <rect
                x={px - DOT_R}
                y={cy - DOT_R}
                width={DOT_R * 2}
                height={DOT_R * 2}
                class={RG_FILL}
                stroke={point === nearestRg ? "currentColor" : "none"}
                stroke-width="1.5"
              />
            {:else if point.dgxProduction}
              <circle cx={px} {cy} {r} fill="var(--primary)" stroke="var(--background)" stroke-width="1.5" />
            {:else}
              <circle cx={px} {cy} r={DOT_R} fill="none" stroke="currentColor" stroke-width="1.5" class="text-muted-foreground" />
            {/if}
            <!-- the hit area: one beeswarm cell (a dot's spacing by the row pitch), so neighbours' targets never overlap -->
            <rect x={px - DOT_R - 1} y={cy - ROW_STEP / 2} width={2 * DOT_R + 2} height={ROW_STEP} fill="transparent" />
          </g>
        {/each}

        <!-- value labels last, with a background halo, so no mark paints over them -->
        {#each labels as { point, px, text, lane, rise } (point.config.id)}
          <text
            x={px}
            y={baseY - rise - 4 - lane * LABEL_LINE}
            font-size="11"
            font-weight={point.dgxProduction ? "600" : "400"}
            fill="currentColor"
            stroke="var(--background)"
            stroke-width="3"
            stroke-linejoin="round"
            paint-order="stroke"
            text-anchor={anchorFor(px)}
            class="pointer-events-none {point.dgxProduction ? 'text-foreground' : 'text-muted-foreground'}"
            aria-hidden="true"
          >
            {text}
          </text>
        {/each}
      </svg>
    {/if}
  </div></div>

  <div class="text-muted-foreground mt-1 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs">
    {#if plotted.production}
      <span class="inline-flex items-center gap-1.5"><span class="bg-primary inline-block size-2.5 rounded-full"></span>DGX production</span>
    {/if}
    {#if plotted.otherDgx}
      <span class="inline-flex items-center gap-1.5"><span class="inline-block size-2.5 rounded-full border-[1.5px] border-current"></span>other DGX</span>
    {/if}
    {#if plotted.rg}
      <span class="inline-flex items-center gap-1.5"><span class="inline-block size-2.5 {RG_SWATCH}"></span>RG GPU</span>
    {/if}
    <span>{points.length} config{points.length === 1 ? "" : "s"}, mean of runs{filters.comparability === "fixed" ? ", fixed harness" : ", all harnesses"}. Tap a dot for details.</span>
  </div>
</section>
