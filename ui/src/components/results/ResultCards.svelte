<script lang="ts">
  import { ChevronRight } from "@lucide/svelte";
  import type { ResultsData } from "../../lib/results";
  import { caveatMarks, cellEntries, textCell, type ColumnDef, type ResultRow } from "../../lib/resultsTable";
  import { Badge } from "$lib/components/ui/badge/index.js";
  import LaneStatus from "./LaneStatus.svelte";
  import Md from "./Md.svelte";

  interface Props {
    data: ResultsData;
    rows: ResultRow[];
    columns: ColumnDef[];
    onselect: (configId: string) => void;
  }

  let { data, rows, columns, onselect }: Props = $props();

  let metricColumns = $derived(columns.filter((c) => c.kind === "metric"));
  let textColumns = $derived(columns.filter((c) => c.kind === "text"));
</script>

<ul class="space-y-2">
  {#each rows as row (row.config.id)}
    <li>
      <button
        type="button"
        class="hover:bg-muted/50 w-full cursor-pointer rounded-lg border p-3 text-left {row.measured ? '' : 'text-muted-foreground'}"
        onclick={() => onselect(row.config.id)}
      >
        <div class="flex items-start justify-between gap-2">
          <div class="min-w-0">
            <div class="text-sm font-medium"><Md inline text={row.config.label} /></div>
            <div class="text-muted-foreground mt-0.5 text-xs">
              {textColumns.map((c) => textCell(row, c.id)).join(" · ")}
            </div>
          </div>
          <ChevronRight class="text-muted-foreground mt-0.5 size-4 shrink-0" />
        </div>
        <div class="mt-2 flex flex-wrap items-center gap-1">
          {#each row.lanes as lane (lane.id)}
            <LaneStatus status={lane.status} title={lane.label} />
          {/each}
          {#each row.harnesses as harness (harness)}
            <Badge variant={harness === "fixed" ? "secondary" : "outline"}>{harness}</Badge>
          {/each}
        </div>
        {#if row.measured}
          <dl class="mt-2 grid grid-cols-2 gap-x-3 gap-y-1.5">
            {#each metricColumns as column (column.id)}
              {@const entries = cellEntries(row, column.id)}
              {#if entries.length > 0}
                <div class="min-w-0">
                  <dt class="text-muted-foreground text-xs">{column.label} <span class="opacity-70">{column.unit}</span></dt>
                  {#each entries as entry (entry.measurement.id)}
                    <dd class="font-mono text-sm tabular-nums">
                      {#if entries.length > 1 && entry.qualifier}<span class="text-muted-foreground block font-sans text-xs">{entry.qualifier}</span>{/if}{entry.text}{#each caveatMarks(data, entry.measurement) as caveat (caveat.mark)}<sup
                          class="text-muted-foreground ml-0.5">{caveat.mark}</sup
                        >{/each}
                    </dd>
                  {/each}
                </div>
              {/if}
            {/each}
          </dl>
        {:else}
          <p class="mt-2 text-xs">No measurements under these filters.</p>
        {/if}
      </button>
    </li>
  {/each}
</ul>
