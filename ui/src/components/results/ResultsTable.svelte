<script lang="ts">
  import { ArrowDown, ArrowUp, ArrowUpDown } from "@lucide/svelte";
  import type { ResultsData } from "../../lib/results";
  import { caveatMarks, cellEntries, firstSortDir, textCell, type ColumnDef, type ResultRow, type SortState } from "../../lib/resultsTable";
  import { Badge } from "$lib/components/ui/badge/index.js";
  import * as Table from "$lib/components/ui/table/index.js";
  import LaneStatus from "./LaneStatus.svelte";
  import Md from "./Md.svelte";

  interface Props {
    data: ResultsData;
    rows: ResultRow[];
    columns: ColumnDef[];
    sort: SortState;
    onsort: (key: string, first: SortState["dir"]) => void;
    onselect: (configId: string) => void;
  }

  let { data, rows, columns, sort, onsort, onselect }: Props = $props();

  function ariaSort(key: string): "ascending" | "descending" | "none" {
    if (sort.key !== key) return "none";
    return sort.dir === "asc" ? "ascending" : "descending";
  }
</script>

{#snippet sortHeader(key: string, label: string, title: string, first: SortState["dir"], unit?: string)}
  <button
    type="button"
    class="hover:text-foreground inline-flex cursor-pointer items-center gap-1 {sort.key === key ? 'text-foreground' : ''}"
    onclick={() => onsort(key, first)}
    {title}
  >
    <span>{label}</span>
    {#if unit}<span class="text-muted-foreground font-normal">{unit}</span>{/if}
    {#if sort.key === key}
      {#if sort.dir === "asc"}<ArrowUp class="size-3.5" />{:else}<ArrowDown class="size-3.5" />{/if}
    {:else}
      <ArrowUpDown class="size-3.5 opacity-30" />
    {/if}
  </button>
{/snippet}

<div class="rounded-lg border">
  <Table.Root>
    <Table.Header>
      <Table.Row>
        <Table.Head aria-sort={ariaSort("config")} class="text-muted-foreground min-w-64">
          {@render sortHeader("config", "Config", "Sort by config label", "asc")}
        </Table.Head>
        {#each columns as column (column.id)}
          <Table.Head
            aria-sort={ariaSort(column.id)}
            class="text-muted-foreground {column.kind === 'metric' ? 'text-right' : ''}"
          >
            {@render sortHeader(column.id, column.label, column.title, firstSortDir(column), column.kind === "metric" ? column.unit : undefined)}
          </Table.Head>
        {/each}
      </Table.Row>
    </Table.Header>
    <Table.Body>
      {#each rows as row (row.config.id)}
        <Table.Row
          class="cursor-pointer {row.measured ? '' : 'text-muted-foreground'}"
          onclick={() => onselect(row.config.id)}
        >
          <Table.Cell class="align-top whitespace-normal">
            <button
              type="button"
              class="cursor-pointer text-left font-medium hover:underline"
              onclick={(e) => {
                e.stopPropagation();
                onselect(row.config.id);
              }}
            >
              <Md inline text={row.config.label} />
            </button>
            <div class="mt-1 flex flex-wrap items-center gap-1">
              <span class="text-muted-foreground mr-1 font-mono text-xs">{row.config.id}</span>
              {#each row.lanes as lane (lane.id)}
                <LaneStatus status={lane.status} title={lane.label} />
              {/each}
              {#each row.harnesses as harness (harness)}
                <Badge variant={harness === "fixed" ? "secondary" : "outline"}>{harness}</Badge>
              {/each}
              {#if !row.measured}<span class="text-xs">no measurements under these filters</span>{/if}
            </div>
          </Table.Cell>
          {#each columns as column (column.id)}
            {#if column.kind === "metric"}
              {@const entries = cellEntries(row, column.id)}
              <Table.Cell class="text-right align-top font-mono text-xs tabular-nums">
                {#if entries.length === 0}
                  <span class="text-muted-foreground">—</span>
                {:else}
                  {#each entries as entry (entry.measurement.id)}
                    <!-- The session label sits on its own line, because inline it widened every
                         multi-session column enough to push Prefill and TTFT off a 1440 px screen. -->
                    <div class="whitespace-nowrap not-first:mt-1">
                      {#if entries.length > 1 && entry.qualifier}<div class="text-muted-foreground font-sans text-[11px] leading-tight">{entry.qualifier}</div>{/if}{entry.text}{#each caveatMarks(data, entry.measurement) as caveat (caveat.mark)}<sup
                          class="text-muted-foreground ml-0.5 cursor-help"
                          title={caveat.text}>{caveat.mark}</sup
                        >{/each}
                    </div>
                  {/each}
                {/if}
              </Table.Cell>
            {:else}
              <Table.Cell class="min-w-24 align-top text-xs whitespace-normal {column.id === 'build' ? 'font-mono' : ''}">{textCell(row, column.id)}</Table.Cell>
            {/if}
          {/each}
        </Table.Row>
      {/each}
    </Table.Body>
  </Table.Root>
</div>
