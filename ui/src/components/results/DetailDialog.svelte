<script lang="ts">
  import { Check, Copy } from "@lucide/svelte";
  import {
    byCodepoint,
    formatMeasurement,
    harnessLabel,
    lanesForConfig,
    metricInfo,
    METRICS,
    predecessorsOf,
    type Measurement,
    type ResultsData,
  } from "../../lib/results";
  import { qualifierOf } from "../../lib/resultsTable";
  import { copyText } from "../../lib/clipboard";
  import { Badge } from "$lib/components/ui/badge/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import CopyableId from "../CopyableId.svelte";
  import LaneStatus from "./LaneStatus.svelte";
  import Md from "./Md.svelte";

  interface Props {
    data: ResultsData;
    configId: string | null;
    onclose: () => void;
    onselect: (configId: string) => void;
  }

  let { data, configId, onclose, onselect }: Props = $props();

  // The clipboard fallback on plain http must live inside the dialog; see copyText.
  let contentEl = $state<HTMLElement | null>(null);
  let copiedFlags = $state(false);

  let config = $derived(configId !== null ? (data.configs[configId] ?? null) : null);
  let host = $derived(config ? data.hosts[config.host] : undefined);
  let build = $derived(config?.build ? data.builds[config.build] : undefined);
  let checkpoint = $derived(config ? data.checkpoints[config.checkpoint] : undefined);
  let lanes = $derived(config ? lanesForConfig(data, config.id) : []);

  const metricOrder = Object.keys(METRICS);
  let measurements = $derived.by(() => {
    if (!config) return [];
    const rank = (m: Measurement) => {
      const i = metricOrder.indexOf(m.metric);
      return i === -1 ? metricOrder.length : i;
    };
    return data.measurements
      .filter((m) => m.config === config.id)
      .sort((a, b) => rank(a) - rank(b) || byCodepoint(a.date ?? "", b.date ?? "") || byCodepoint(a.id, b.id));
  });
  let predecessors = $derived(predecessorsOf(data, new Set(measurements.map((m) => m.id))));
  // A lane's current config with no numbers of its own points at the config that was measured.
  let measuredElsewhere = $derived(
    lanes.filter((lane) => lane.current_config === config?.id && lane.measured_config && lane.measured_config !== config?.id),
  );

  $effect(() => {
    void configId;
    copiedFlags = false;
  });

  async function copyFlags(): Promise<void> {
    if (!config || !(await copyText(config.flags.join("\n"), contentEl))) return;
    copiedFlags = true;
    setTimeout(() => (copiedFlags = false), 1500);
  }

  /** What the spec label doesn't already say: the method (unless named in it), depth, draft vocab and draft. */
  function specDetail(): string {
    if (!config) return "";
    const parts: string[] = [];
    if (!config.spec.label.toLowerCase().includes(config.spec.method.toLowerCase())) parts.push(config.spec.method);
    if (config.spec.depth !== undefined) parts.push(`depth ${config.spec.depth}`);
    if (config.spec.draft_vocab !== undefined) parts.push(`draft vocab ${config.spec.draft_vocab.toLocaleString("en-US")}`);
    if (config.spec.draft_checkpoint) parts.push(`draft ${data.checkpoints[config.spec.draft_checkpoint]?.name ?? config.spec.draft_checkpoint}`);
    return parts.join(", ");
  }

  function conditionsText(m: Measurement): string {
    return Object.entries(m.conditions)
      .map(([key, value]) => `${key.replaceAll("_", " ")} ${typeof value === "number" ? value.toLocaleString("en-US") : value}`)
      .join(" · ");
  }

  function thinkingText(m: Measurement): string | null {
    if (m.thinking_measured === undefined) return null;
    return m.thinking_measured ? "thinking on" : "thinking off";
  }
</script>

{#snippet field(label: string, value: string | undefined | null)}
  {#if value}
    <div class="grid grid-cols-[7.5rem_1fr] gap-2 py-1 sm:grid-cols-[9rem_1fr]">
      <dt class="text-muted-foreground">{label}</dt>
      <dd class="min-w-0 break-words">{value}</dd>
    </div>
  {/if}
{/snippet}

{#snippet sectionTitle(text: string)}
  <h4 class="text-muted-foreground mb-2 text-xs font-semibold tracking-wider uppercase">{text}</h4>
{/snippet}

<Dialog.Root
  open={configId !== null}
  onOpenChange={(v) => {
    if (!v) onclose();
  }}
>
  <Dialog.Content bind:ref={contentEl} class="flex max-h-[90vh] w-[95%] flex-col gap-0 p-0 sm:max-w-3xl">
    {#if config}
      <Dialog.Header class="border-border border-b px-4 py-3">
        <Dialog.Title class="pr-6 text-base font-semibold"><Md inline text={config.label} /></Dialog.Title>
        <Dialog.Description class="flex flex-wrap items-center gap-1.5">
          <CopyableId value={config.id} class="font-mono text-xs" />
          {#each lanes as lane (lane.id)}<LaneStatus status={lane.status} title={lane.label} />{/each}
        </Dialog.Description>
      </Dialog.Header>

      <div class="min-h-0 flex-1 space-y-5 overflow-y-auto p-4 text-sm">
        <!-- Measurements first: the numbers are what the dialog is opened for. -->
        <section>
          {@render sectionTitle(`Measurements (${measurements.length})`)}
          {#if measurements.length === 0}
            <p class="text-muted-foreground">No measurements recorded for this config.</p>
          {:else}
            <div class="space-y-2">
              {#each measurements as m (m.id)}
                {@const info = metricInfo(m.metric)}
                <div class="rounded-md border p-3 {m.superseded_by ? 'opacity-75' : ''}">
                  <div class="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1">
                    <div class="font-medium">
                      {info.label}{#if qualifierOf(m)}<span class="text-muted-foreground font-normal">{` · ${qualifierOf(m)}`}</span>{/if}
                    </div>
                    <div class="font-mono tabular-nums">
                      {formatMeasurement(m)} <span class="text-muted-foreground font-sans text-xs">{m.unit}</span>
                    </div>
                  </div>
                  <div class="mt-1.5 flex flex-wrap items-center gap-1">
                    <Badge variant={m.harness === "fixed-2026-09-27" ? "secondary" : "outline"}>{harnessLabel(m.harness)}</Badge>
                    {#if thinkingText(m)}<Badge variant="outline">{thinkingText(m)}</Badge>{/if}
                    {#if m.access}<Badge variant="outline">{m.access}</Badge>{/if}
                    {#if m.residency}<Badge variant="outline">{m.residency}</Badge>{/if}
                    {#if m.decode_definition}<Badge variant="outline">{m.decode_definition}</Badge>{/if}
                    {#if m.superseded_by}<Badge variant="destructive">superseded</Badge>{/if}
                  </div>
                  <p class="text-muted-foreground mt-1.5 text-xs">
                    {[
                      m.runs && m.runs.length > 1 ? `${m.runs.length} runs, mean ${formatMeasurement(m, { aggregate: "mean" })}` : null,
                      m.range ? "range only" : null,
                      conditionsText(m) || null,
                      m.date,
                      m.campaign ? (data.campaigns[m.campaign]?.label ?? m.campaign) : null,
                      m.harness_commit ? `harness ${m.harness_commit}` : null,
                    ]
                      .filter(Boolean)
                      .join(" · ")}
                  </p>
                  {#if m.caveats.length > 0}
                    <ul class="mt-2 space-y-1 text-xs">
                      {#each m.caveats as id (id)}
                        <li class="flex gap-1.5">
                          <span class="text-muted-foreground shrink-0">{data.caveats[id]?.mark ?? "•"}</span>
                          {#if data.caveats[id]}<Md text={data.caveats[id].text_md} />{:else}<span class="font-mono">{id}</span>{/if}
                        </li>
                      {/each}
                    </ul>
                  {/if}
                  {#if m.note}<Md class="mt-2 text-xs" text={m.note} />{/if}
                  {#if m.evidence.length > 0 || m.history_section || m.superseded_by}
                    <dl class="mt-2 space-y-0.5 text-xs">
                      {#each m.evidence as path (path)}
                        <div class="flex gap-2"><dt class="text-muted-foreground w-16 shrink-0">evidence</dt><dd class="min-w-0"><CopyableId value={path} class="font-mono" /></dd></div>
                      {/each}
                      {#if m.history_section}
                        <div class="flex gap-2"><dt class="text-muted-foreground w-16 shrink-0">history</dt><dd class="min-w-0"><CopyableId value={m.history_section} /></dd></div>
                      {/if}
                      {#if m.superseded_by}
                        <div class="flex gap-2"><dt class="text-muted-foreground w-16 shrink-0">replaced by</dt><dd class="min-w-0"><CopyableId value={m.superseded_by} class="font-mono" /></dd></div>
                      {/if}
                    </dl>
                  {/if}
                </div>
              {/each}
            </div>
          {/if}
        </section>

        <section>
          {@render sectionTitle("Configuration")}
          <dl class="divide-border divide-y">
            {@render field("Model", config.model)}
            {@render field("Family", config.family)}
            {@render field("Host", host ? `${host.label} · ${host.gpu}${host.memory_gb ? ` · ${host.memory_gb} GB` : ""}` : config.host)}
            {@render field("Engine", config.engine_label ? `${config.engine_label} (${config.engine})` : config.engine)}
            <div class="grid grid-cols-[7.5rem_1fr] gap-2 py-1 sm:grid-cols-[9rem_1fr]">
              <dt class="text-muted-foreground">Build</dt>
              <dd class="min-w-0 space-y-1">
                {#if config.build}
                  <CopyableId value={config.build} class="font-mono text-xs" />
                  {#if build}
                    <Md text={build.version} />
                    {#if build.engine}<p class="text-muted-foreground text-xs">{build.engine}</p>{/if}
                    {#if build.torch || build.flashinfer}
                      <p class="text-muted-foreground text-xs">torch {build.torch ?? "?"} · flashinfer {build.flashinfer ?? "?"}</p>
                    {/if}
                  {:else}
                    <p class="text-muted-foreground text-xs">Build key not in the catalog.</p>
                  {/if}
                {:else}
                  <span class="text-muted-foreground">unrecorded</span>
                {/if}
              </dd>
            </div>
            {#if config.image || config.image_digest}
              <div class="grid grid-cols-[7.5rem_1fr] gap-2 py-1 sm:grid-cols-[9rem_1fr]">
                <dt class="text-muted-foreground">Image</dt>
                <dd class="min-w-0 space-y-0.5">
                  {#if config.image}<CopyableId value={config.image} class="font-mono text-xs" />{/if}
                  {#if config.image_digest}<CopyableId value={config.image_digest} class="font-mono text-xs" />{/if}
                </dd>
              </div>
            {/if}
            <div class="grid grid-cols-[7.5rem_1fr] gap-2 py-1 sm:grid-cols-[9rem_1fr]">
              <dt class="text-muted-foreground">Checkpoint</dt>
              <dd class="min-w-0 space-y-0.5">
                <div>{checkpoint?.name ?? config.checkpoint}{checkpoint?.precision ? ` · ${checkpoint.precision}` : ""}</div>
                {#if checkpoint?.source}<Md class="text-muted-foreground text-xs" text={checkpoint.source} />{/if}
                {#if checkpoint?.revision && !checkpoint.source?.includes(checkpoint.revision)}<CopyableId value={checkpoint.revision} class="font-mono text-xs" />{/if}
                {#if checkpoint?.notes_md}<Md class="text-muted-foreground text-xs" text={checkpoint.notes_md} />{/if}
              </dd>
            </div>
            {@render field("KV cache", config.kv_dtype)}
            {@render field("Spec decode", `${config.spec.label}${specDetail() ? ` (${specDetail()})` : ""}`)}
            {@render field("Context", config.context?.toLocaleString("en-US"))}
            {@render field("Thinking default", config.thinking_default)}
            {#if config.compose}
              <div class="grid grid-cols-[7.5rem_1fr] gap-2 py-1 sm:grid-cols-[9rem_1fr]">
                <dt class="text-muted-foreground">Compose / profile</dt>
                <dd class="min-w-0"><CopyableId value={config.compose} class="font-mono text-xs" /></dd>
              </div>
            {/if}
            {@render field("Matrix id", config.matrix_id)}
          </dl>
          {#if config.notes_md}<Md class="mt-2" text={config.notes_md} />{/if}
          {#if build?.notes_md}
            <details class="mt-2">
              <summary class="text-muted-foreground hover:text-foreground cursor-pointer text-xs">Build notes</summary>
              <Md class="mt-1 text-xs" text={build.notes_md} />
              {#if build.image}<Md class="text-muted-foreground mt-1 text-xs" text={build.image} />{/if}
            </details>
          {/if}
        </section>

        {#if config.flags.length > 0 || Object.keys(config.env).length > 0}
          <section>
            <div class="mb-2 flex items-center justify-between gap-2">
              <h4 class="text-muted-foreground text-xs font-semibold tracking-wider uppercase">Flags</h4>
              {#if config.flags.length > 0}
                <Button variant="outline" size="xs" onclick={() => void copyFlags()}>
                  {#if copiedFlags}<Check />Copied{:else}<Copy />Copy all{/if}
                </Button>
              {/if}
            </div>
            <ul class="bg-muted/40 space-y-0.5 rounded-md border p-2 font-mono text-xs">
              {#each config.flags as flag, i (i)}
                <li><CopyableId value={flag} /></li>
              {/each}
              {#each Object.entries(config.env) as [key, value] (key)}
                <li><CopyableId value="{key}={value}" /> <span class="text-muted-foreground font-sans">env</span></li>
              {/each}
            </ul>
          </section>
        {/if}

        {#if lanes.length > 0}
          <section>
            {@render sectionTitle(lanes.length === 1 ? "Lane" : "Lanes")}
            <div class="space-y-3">
              {#each lanes as lane (lane.id)}
                <div class="rounded-md border p-3">
                  <div class="flex flex-wrap items-center gap-2">
                    <span class="font-medium">{lane.label}</span>
                    <LaneStatus status={lane.status} />
                  </div>
                  <p class="text-muted-foreground mt-1 text-xs">
                    {[
                      lane.since ? `since ${lane.since}` : null,
                      lane.llama_swap_id ? `llama-swap ${lane.llama_swap_id}` : null,
                      lane.rg_profile ? `RG profile ${lane.rg_profile}` : null,
                      lane.port ? `127.0.0.1:${lane.port}` : null,
                      lane.current_config !== config.id ? `serves ${lane.current_config}` : null,
                    ]
                      .filter(Boolean)
                      .join(" · ")}
                  </p>
                  {#if lane.notes_md}<Md class="mt-2" text={lane.notes_md} />{/if}
                </div>
              {/each}
            </div>
            {#each measuredElsewhere as lane (lane.id)}
              <p class="text-muted-foreground mt-2 text-xs">
                This lane's numbers were measured on
                <button type="button" class="text-foreground cursor-pointer font-mono underline" onclick={() => onselect(lane.measured_config!)}
                  >{lane.measured_config}</button
                >; this config hasn't been re-measured.
              </p>
            {/each}
          </section>
        {/if}

        {#if predecessors.length > 0}
          <section>
            {@render sectionTitle("Superseded predecessors")}
            <ul class="space-y-1.5 text-xs">
              {#each predecessors as m (m.id)}
                <li class="flex flex-wrap items-baseline gap-x-2">
                  <button type="button" class="cursor-pointer font-mono underline" onclick={() => onselect(m.config)}>{m.id}</button>
                  <span class="font-mono tabular-nums">{formatMeasurement(m)} {m.unit}</span>
                  <span class="text-muted-foreground">{[m.date, harnessLabel(m.harness), `replaced by ${m.superseded_by}`].filter(Boolean).join(" · ")}</span>
                </li>
              {/each}
            </ul>
          </section>
        {/if}

        {#if config.caveats.length > 0}
          <section>
            {@render sectionTitle("Config caveats")}
            <ul class="space-y-1 text-xs">
              {#each config.caveats as id (id)}
                <li class="flex gap-1.5">
                  <span class="text-muted-foreground shrink-0">{data.caveats[id]?.mark ?? "•"}</span>
                  {#if data.caveats[id]}<Md text={data.caveats[id].text_md} />{:else}<span class="font-mono">{id}</span>{/if}
                </li>
              {/each}
            </ul>
          </section>
        {/if}
      </div>

      <!-- A plain div, not Dialog.Footer: that one's negative margins assume a padded Content, and this one is p-0. -->
      <div class="border-border flex justify-end border-t px-4 py-3">
        <Button variant="outline" onclick={onclose}>Close</Button>
      </div>
    {:else if configId !== null}
      <div class="p-8 text-center">
        <p class="text-muted-foreground">Config {configId} isn't in the catalog.</p>
        <Button class="mt-4" variant="outline" onclick={onclose}>Close</Button>
      </div>
    {/if}
  </Dialog.Content>
</Dialog.Root>
