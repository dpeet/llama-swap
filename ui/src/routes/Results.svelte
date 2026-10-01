<script lang="ts">
  import { onMount, untrack } from "svelte";
  import { querystring, replace } from "svelte-spa-router";
  import { RefreshCw, X } from "@lucide/svelte";
  import { getResults, ResultsApiError } from "../stores/api";
  import { persistentStore } from "../stores/persistent";
  import { normalizeResults, type ResultsData } from "../lib/results";
  import {
    comparePoints,
    configsShown,
    emptyResultsFilters,
    resultsFiltersFromQuery,
    resultsFiltersToQuery,
    shownMeasurements,
    type ResultsFilters,
  } from "../lib/resultsFilters";
  import {
    buildRows,
    DEFAULT_VISIBLE_COLUMNS,
    metricColumns,
    nextSort,
    normalizeVisibleColumns,
    sortRows,
    TEXT_COLUMNS,
    type SortState,
  } from "../lib/resultsTable";
  import { IsMobile } from "$lib/hooks/is-mobile.svelte.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import CompareStrip from "../components/results/CompareStrip.svelte";
  import FilterBar from "../components/results/FilterBar.svelte";
  import ResultsTable from "../components/results/ResultsTable.svelte";
  import ResultCards from "../components/results/ResultCards.svelte";
  import DetailDialog from "../components/results/DetailDialog.svelte";

  let data = $state<ResultsData | null>(null);
  let loading = $state(true);
  let error = $state<Error | null>(null);
  let rejectedDismissed = $state(false);

  async function load(): Promise<void> {
    loading = true;
    try {
      const files = await getResults();
      data = normalizeResults(files.catalog, files.measurementsText);
      error = null;
      rejectedDismissed = false;
    } catch (cause) {
      error = cause instanceof Error ? cause : new Error(String(cause));
    } finally {
      loading = false;
    }
  }

  onMount(() => {
    void load();
  });

  // ---- Filters live in the hash querystring (plan D8), so a view can be shared or bookmarked.
  let filters = $state<ResultsFilters>(resultsFiltersFromQuery(untrack(() => $querystring)));
  // Querystrings this page wrote that haven't come back through the router yet:
  // replace() lands a tick later, so a fast typist's earlier write must not be
  // read back as a navigation and overwrite newer input.
  const pendingWrites: string[] = [];

  $effect(() => {
    const qs = $querystring ?? "";
    const pending = pendingWrites.indexOf(qs);
    if (pending !== -1) {
      pendingWrites.splice(0, pending + 1);
      return;
    }
    if (qs !== resultsFiltersToQuery(untrack(() => filters))) filters = resultsFiltersFromQuery(qs);
  });

  function setFilters(next: ResultsFilters): void {
    filters = next;
    const qs = resultsFiltersToQuery(next);
    if (qs === ($querystring ?? "") && pendingWrites.length === 0) return;
    pendingWrites.push(qs);
    void replace(qs ? `/results?${qs}` : "/results");
  }

  function update(patch: Partial<ResultsFilters>): void {
    setFilters({ ...filters, ...patch });
  }

  function resetFilters(): void {
    // Keep the comparison itself; reset only what narrows the table.
    setFilters({ ...emptyResultsFilters(), compareMetric: filters.compareMetric, yours: filters.yours });
  }

  // ---- Columns (persisted per browser) and sort
  const visibleColumnsStore = persistentStore<string[]>("results-visible-columns", DEFAULT_VISIBLE_COLUMNS);
  let visibleColumnIds = $derived(normalizeVisibleColumns($visibleColumnsStore));
  let sort = $state<SortState>({ key: "", dir: "asc" });
  const isMobile = new IsMobile();

  function toggleColumn(id: string, visible: boolean): void {
    const current = normalizeVisibleColumns($visibleColumnsStore).filter((c) => c !== id);
    visibleColumnsStore.set(visible ? [...current, id] : current);
  }

  // ---- Derived view of the data
  let allColumns = $derived(data ? [...TEXT_COLUMNS, ...metricColumns(data)] : TEXT_COLUMNS);
  // In the menu's order, not click order, so toggling a column never reshuffles the table.
  let visibleColumns = $derived(allColumns.filter((c) => visibleColumnIds.includes(c.id)));
  let shown = $derived(data ? shownMeasurements(data, filters) : new Map());
  let configs = $derived(data ? configsShown(data, filters, shown) : []);
  let rows = $derived(data ? sortRows(buildRows(data, configs, shown), sort, allColumns) : []);
  // The strip ignores the table's metric filter (a different metric would empty it) but honours the rest.
  let stripConfigs = $derived(data ? configsShown(data, { ...filters, metric: "" }, shown) : []);
  let points = $derived(data ? comparePoints(data, filters, stripConfigs) : []);

  function options(values: Iterable<[string, string]>): { value: string; label: string }[] {
    return [...new Map(values)].map(([value, label]) => ({ value, label })).sort((a, b) => a.label.localeCompare(b.label));
  }
  let configList = $derived(data ? Object.values(data.configs) : []);
  let families = $derived([...new Set(configList.map((c) => c.family))].sort());
  let hostOptions = $derived(options(configList.map((c) => [c.host, data?.hosts[c.host]?.label ?? c.host])));
  let engineOptions = $derived(options(configList.map((c) => [c.engine, c.engine])));
  let metricOptions = $derived(data ? metricColumns(data).map((c) => ({ value: c.id, label: c.title })) : []);
  let statusOptions = $derived(options(Object.values(data?.lanes ?? {}).map((l) => [l.status, l.status])));

  // ---- Detail dialog
  let selectedId = $state<string | null>(null);

  // ---- Error card copy, per the server's codes (internal/server/results.go)
  let errorCode = $derived(error instanceof ResultsApiError ? error.code : null);
  let errorTitle = $derived.by(() => {
    if (errorCode === "results_not_configured") return "Results not configured";
    if (errorCode === "results_file_missing") return "Results file missing";
    if (errorCode === "results_read_failed") return "Could not read the results files";
    if (errorCode === "bad_response") return "Unexpected response";
    return "Could not load results";
  });
  let errorHint = $derived.by(() => {
    if (errorCode === "results_not_configured")
      return "llama-swap has no LLAMA_SWAP_RESULTS_DIR, so it has no results directory to serve. Set it to artisanal-inference's docs/results in serve/compose.llama-swap.yaml.";
    if (errorCode === "results_file_missing")
      return "LLAMA_SWAP_RESULTS_DIR is set, but catalog.json or measurements.jsonl isn't in it. Check the path and the read-only mount.";
    if (errorCode === "results_read_failed") return "The files exist but llama-swap couldn't read them; check their permissions.";
    if (errorCode === "bad_response") return "Something other than llama-swap answered, or catalog.json isn't valid JSON.";
    return "";
  });
</script>

<div class="p-2">
  <div class="mt-4 mb-4 flex items-start justify-between gap-3">
    <div>
      <h3 class="text-lg font-semibold">Results</h3>
      <p class="text-muted-foreground text-sm">
        {#if data}
          {Object.keys(data.configs).length} configs, {data.measurements.length} measurements. Compare a quoted number with the DGX and the RG GPUs.
        {:else}
          Benchmark results from artisanal-inference docs/results.
        {/if}
      </p>
    </div>
    <Button variant="outline" size="sm" onclick={() => void load()} disabled={loading} aria-label="Refresh">
      <RefreshCw class={loading ? "animate-spin" : ""} />
      Refresh
    </Button>
  </div>

  {#if error && !data}
    <div class="border-destructive/50 rounded-lg border p-6">
      <h4 class="font-semibold">{errorTitle}</h4>
      {#if errorHint}<p class="text-muted-foreground mt-1 text-sm">{errorHint}</p>{/if}
      {#if error.message}<p class="text-muted-foreground mt-1 font-mono text-xs">{error.message}</p>{/if}
    </div>
  {:else if !data}
    <div class="text-muted-foreground rounded-lg border p-6 text-sm">Loading results…</div>
  {:else}
    {#if error}
      <div class="border-destructive/50 mb-4 rounded-lg border p-3 text-sm">
        <span class="font-semibold">{errorTitle}.</span> Showing the last data. <span class="font-mono text-xs">{error.message}</span>
      </div>
    {/if}

    {#if data.rejected.length > 0 && !rejectedDismissed}
      <div class="mb-4 flex items-start gap-2 rounded-lg border border-amber-500/50 p-3 text-sm">
        <div class="min-w-0 flex-1">
          <span class="font-semibold">
            {data.rejected.length} record{data.rejected.length === 1 ? "" : "s"} skipped.
          </span>
          <span class="text-muted-foreground">They're left out of the page; <code class="font-mono text-xs">results.py lint</code> names the fix.</span>
          <ul class="mt-1 max-h-40 space-y-0.5 overflow-y-auto font-mono text-xs">
            {#each data.rejected as item, i (i)}
              <li class="break-all"><span class="font-semibold">{item.id}</span>: {item.reason}</li>
            {/each}
          </ul>
        </div>
        <Button variant="ghost" size="icon-sm" onclick={() => (rejectedDismissed = true)} aria-label="Dismiss skipped records">
          <X />
        </Button>
      </div>
    {/if}

    {#if Object.keys(data.configs).length === 0}
      <div class="text-muted-foreground rounded-lg border p-6 text-sm">
        <p class="text-foreground font-semibold">No results yet</p>
        <p class="mt-1">catalog.json has no configs. They appear here once records are added to docs/results/.</p>
      </div>
    {:else}
      <CompareStrip {data} {filters} {points} {families} onchange={update} onselect={(id) => (selectedId = id)} />

      <div class="mt-4">
        <FilterBar
          {filters}
          hosts={hostOptions}
          engines={engineOptions}
          metrics={metricOptions}
          statuses={statusOptions}
          columns={allColumns}
          visibleColumns={visibleColumnIds}
          shownCount={rows.length}
          totalCount={configList.length}
          onchange={update}
          onreset={resetFilters}
          ontogglecolumn={toggleColumn}
        />
      </div>

      <div class="mt-3">
        {#if rows.length === 0}
          <div class="text-muted-foreground rounded-lg border p-6 text-center text-sm">
            <p>No configs match these filters.</p>
            <Button class="mt-3" variant="outline" size="sm" onclick={resetFilters}>Reset filters</Button>
          </div>
        {:else if isMobile.current}
          <ResultCards {data} {rows} columns={visibleColumns} onselect={(id) => (selectedId = id)} />
        {:else}
          <ResultsTable
            {data}
            {rows}
            columns={visibleColumns}
            {sort}
            onsort={(key, first) => (sort = nextSort(sort, key, first))}
            onselect={(id) => (selectedId = id)}
          />
        {/if}
      </div>
    {/if}
  {/if}
</div>

{#if data}
  <DetailDialog {data} configId={selectedId} onclose={() => (selectedId = null)} onselect={(id) => (selectedId = id)} />
{/if}
