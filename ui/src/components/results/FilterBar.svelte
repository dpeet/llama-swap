<script lang="ts">
  import { Columns3, Search, X } from "@lucide/svelte";
  import { activeFilterCount, UNSERVED, type ResultsFilters, type Thinking } from "../../lib/resultsFilters";
  import type { ColumnDef } from "../../lib/resultsTable";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import { Switch } from "$lib/components/ui/switch/index.js";
  import * as Select from "$lib/components/ui/select/index.js";
  import * as DropdownMenu from "$lib/components/ui/dropdown-menu/index.js";

  interface Option {
    value: string;
    label: string;
  }

  interface Props {
    filters: ResultsFilters;
    hosts: Option[];
    engines: Option[];
    metrics: Option[];
    statuses: Option[];
    columns: ColumnDef[];
    visibleColumns: string[];
    shownCount: number;
    totalCount: number;
    onchange: (patch: Partial<ResultsFilters>) => void;
    onreset: () => void;
    ontogglecolumn: (id: string, visible: boolean) => void;
  }

  let {
    filters,
    hosts,
    engines,
    metrics,
    statuses,
    columns,
    visibleColumns,
    shownCount,
    totalCount,
    onchange,
    onreset,
    ontogglecolumn,
  }: Props = $props();

  // bits-ui Select can't hold "", so "any" stands for an unset filter.
  const ANY = "__any__";
  const THINKING_OPTIONS: Option[] = [
    { value: "on", label: "Thinking on" },
    { value: "off", label: "Thinking off" },
  ];

  let count = $derived(activeFilterCount(filters));
  let statusOptions = $derived([...statuses, { value: UNSERVED, label: "no lane" }]);
</script>

{#snippet pick(id: string, anyLabel: string, value: string, options: Option[], onpick: (v: string) => void)}
  <Select.Root type="single" value={value === "" ? ANY : value} onValueChange={(v) => v && onpick(v === ANY ? "" : v)}>
    <Select.Trigger {id} size="sm" class="max-w-48 {value === '' ? 'text-muted-foreground' : ''}" aria-label={anyLabel}>
      <span class="truncate">{value === "" ? anyLabel : (options.find((o) => o.value === value)?.label ?? value)}</span>
    </Select.Trigger>
    <Select.Content>
      <Select.Item value={ANY}>{anyLabel}</Select.Item>
      {#each options as option (option.value)}
        <Select.Item value={option.value}>{option.label}</Select.Item>
      {/each}
    </Select.Content>
  </Select.Root>
{/snippet}

<div class="space-y-2">
  <div class="flex flex-wrap items-center gap-2">
    <div class="relative min-w-0 flex-1 basis-56">
      <Search class="text-muted-foreground pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2" />
      <Input
        type="search"
        class="pl-8"
        placeholder="Search configs, flags, builds, caveats…"
        aria-label="Search results"
        value={filters.search}
        oninput={(e) => onchange({ search: e.currentTarget.value })}
      />
    </div>
    <DropdownMenu.Root>
      <DropdownMenu.Trigger>
        {#snippet child({ props })}
          <Button {...props} variant="outline" size="sm" title="Choose columns">
            <Columns3 />
            <span class="hidden sm:inline">Columns</span>
          </Button>
        {/snippet}
      </DropdownMenu.Trigger>
      <DropdownMenu.Content align="end" class="max-h-[60vh] min-w-56 overflow-y-auto">
        {#each columns as column (column.id)}
          <DropdownMenu.CheckboxItem
            checked={visibleColumns.includes(column.id)}
            onCheckedChange={(v) => ontogglecolumn(column.id, !!v)}
            closeOnSelect={false}
          >
            <span class="flex-1">{column.title}</span>
          </DropdownMenu.CheckboxItem>
        {/each}
      </DropdownMenu.Content>
    </DropdownMenu.Root>
  </div>

  <div class="flex flex-wrap items-center gap-2">
    {@render pick("results-host", "Any host", filters.host, hosts, (v) => onchange({ host: v }))}
    {@render pick("results-engine", "Any engine", filters.engine, engines, (v) => onchange({ engine: v }))}
    {@render pick("results-metric", "Any metric", filters.metric, metrics, (v) => onchange({ metric: v }))}
    {@render pick("results-thinking", "Thinking on or off", filters.thinking === "any" ? "" : filters.thinking, THINKING_OPTIONS, (v) =>
      onchange({ thinking: (v === "" ? "any" : v) as Thinking }),
    )}
    {@render pick("results-status", "Any lane status", filters.status, statusOptions, (v) => onchange({ status: v }))}

    <label class="flex items-center gap-2 text-sm" title="Off shows only the fixed 2026-09-27 harness, the one whose numbers compare across hosts">
      <Switch
        size="sm"
        checked={filters.comparability === "all"}
        onCheckedChange={(v) => onchange({ comparability: v ? "all" : "fixed" })}
      />
      All harnesses
    </label>
    <label class="flex items-center gap-2 text-sm">
      <Switch size="sm" checked={filters.showSuperseded} onCheckedChange={(v) => onchange({ showSuperseded: v })} />
      Superseded
    </label>

    <span class="text-muted-foreground ml-auto text-xs whitespace-nowrap">
      {shownCount} of {totalCount} configs
    </span>
    {#if count > 0}
      <Button variant="ghost" size="sm" onclick={onreset}>
        <X />
        Reset {count === 1 ? "filter" : `${count} filters`}
      </Button>
    {/if}
  </div>
</div>
