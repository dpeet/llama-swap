<script lang="ts">
  import { onMount } from "svelte";
  import { RefreshCw } from "@lucide/svelte";
  import { getRgOverview, grabRg, releaseRg, RgApiError } from "../stores/api";
  import type { RgFamilyChoice, RgGrabResponse, RgHold, RgNode, RgOverview } from "../lib/types";
  import { canonicalDuration, familyLabel, familyOptions, followTarget, forceReleaseOffered, formatClock, holdStatus, holdsUnavailable, idleBadgeShown, parseDuration, refreshDue, rgActionErrorText, advanceIso, elapsedSeconds, timeLeftText, REFRESH_INTERVAL_MS } from "../lib/rg";
  import RgNodeCard from "../components/RgNodeCard.svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Badge } from "$lib/components/ui/badge/index.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import * as Card from "$lib/components/ui/card/index.js";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import * as Select from "$lib/components/ui/select/index.js";

  // D35 (2026-09-29): the overview is fetched on navigation, on the refresh
  // button, once after a grab or release, and every 2 min while the tab is
  // visible. Between fetches a 30 s tick only re-renders the time-left labels
  // (no request), advancing the last overview by the time since it was fetched.
  let overview = $state<RgOverview | null>(null);
  let loading = $state(true);
  let error = $state<RgApiError | Error | null>(null);

  const COUNTDOWN_TICK_MS = 30_000;
  // Client clock of the last successful fetch, and the ticking "now". Elapsed
  // time is client-vs-client, so a skewed server clock cannot distort the countdown.
  let fetchedAt = $state<number | null>(null);
  let now = $state(Date.now());
  let elapsedS = $derived(fetchedAt === null ? 0 : elapsedSeconds(fetchedAt, now));
  // The snapshot time advanced to now, the reference for start-by bounds.
  let busyNow = $derived(advanceIso(overview?.generated_at, elapsedS));

  // Plain flags, not $state: they gate fetches and drive no rendering.
  let inFlight = false;
  let reloadQueued = false;
  let lastAttemptAt: number | null = null;

  // Never overlaps two fetches: a request made while one is in flight (a grab's
  // refresh, a manual refresh) is queued and runs once when it lands, so the
  // page never keeps a response that predates the action.
  async function load(): Promise<void> {
    if (inFlight) {
      reloadQueued = true;
      return;
    }
    inFlight = true;
    loading = true;
    lastAttemptAt = Date.now();
    try {
      overview = await getRgOverview();
      fetchedAt = Date.now();
      now = fetchedAt;
      error = null;
      syncFamily();
    } catch (cause) {
      error = cause instanceof Error ? cause : new Error(String(cause));
    } finally {
      inFlight = false;
      loading = false;
      if (reloadQueued) {
        reloadQueued = false;
        void load();
      }
    }
  }

  // lastAttemptAt, not fetchedAt, so a failing rg-api is retried every 2 min
  // rather than on every visibility flip.
  function refreshIfDue(): void {
    now = Date.now();
    if (refreshDue({ visible: document.visibilityState === "visible", inFlight, lastFetchAt: lastAttemptAt, now })) {
      void load();
    }
  }

  onMount(() => {
    void load();
    const countdown = setInterval(() => (now = Date.now()), COUNTDOWN_TICK_MS);
    const refresh = setInterval(refreshIfDue, REFRESH_INTERVAL_MS);
    // Timers are throttled in hidden tabs, so catch up on return.
    document.addEventListener("visibilitychange", refreshIfDue);
    return () => {
      clearInterval(countdown);
      clearInterval(refresh);
      document.removeEventListener("visibilitychange", refreshIfDue);
    };
  });

  let errorTitle = $derived.by(() => {
    const code = error instanceof RgApiError ? error.code : null;
    if (code === "rg_api_not_configured") return "RG API not configured";
    if (code === "rg_api_unreachable") return "RG API unreachable";
    return "Could not load RG GPUs";
  });
  let errorHint = $derived.by(() => {
    const code = error instanceof RgApiError ? error.code : null;
    if (code === "rg_api_not_configured") return "llama-swap has no LLAMA_SWAP_RG_API_URL, so it has nowhere to send /api/rg requests.";
    if (code === "rg_api_unreachable") return "llama-swap could not reach rg-api. It may be stopped: check the rg-api tmux session on the host.";
    return "";
  });

  // ---- Grab form ----------------------------------------------------------

  const BEST = "best";
  const NEXT_GH200 = "next-gh200";
  let nodeChoice = $state(BEST);
  let family = $state<RgFamilyChoice>("flash-next");
  let duration = $state("2h");
  let grabbing = $state(false);
  let grabResult = $state<{ ok: boolean; text: string } | null>(null);

  // Best available can serve any family some node has a profile for.
  let selectedNode = $derived<RgNode | null>(overview?.nodes.find((n) => n.name === nodeChoice) ?? null);
  let choiceNodes = $derived(selectedNode ? [selectedNode] : (overview?.nodes ?? []).filter((n) => nodeChoice !== NEXT_GH200 || n.gpu === "GH200"));
  let families = $derived(
    familyOptions({ profiles: choiceNodes.flatMap((n) => n.profiles) }),
  );

  function syncFamily(): void {
    if (!families.some((o) => o.value === family)) {
      family = families.find((o) => o.isDefault)?.value ?? "hold-only";
    }
  }

  // MaxTime for the chosen node, or the largest one for best available
  // (rg-api filters the rest).
  let maxMinutes = $derived.by(() => {
    const nodes = choiceNodes;
    const limits = nodes.map((n) => parseDuration(n.max_time)).filter((m): m is number => m !== null);
    return limits.length > 0 ? Math.max(...limits) : null;
  });
  let maxTimeLabel = $derived(
    selectedNode?.max_time ?? choiceNodes.map((n) => n.max_time).sort((a, b) => (parseDuration(b) ?? 0) - (parseDuration(a) ?? 0))[0] ?? "",
  );

  let durationError = $derived.by(() => {
    const minutes = parseDuration(duration);
    if (minutes === null) return "Use 2h, 90m or 1h30m, at least 30m.";
    if (maxMinutes !== null && minutes > maxMinutes) return `Longer than the ${maxTimeLabel} limit.`;
    return "";
  });

  function bestLabel(): string {
    const best = family === "hold-only" ? null : (overview?.best[family] ?? null);
    if (family === "hold-only") return "Best available";
    return `Best available · ${best ?? "none idle"}`;
  }
  let nodeLabel = $derived(nodeChoice === BEST ? bestLabel() : nodeChoice === NEXT_GH200 ? "Next available GH200" : nodeChoice);

  // A named node where we already hold queues the grab behind that hold, because
  // rg-hold.sh refuses a second hold there otherwise (DUPLICATE).
  let followJob = $derived(selectedNode ? followTarget(holdsOn(selectedNode.name)) : null);
  // Because afterany only makes a follow-on eligible, another user already queued for the node can start first.
  let othersQueued = $derived(
    selectedNode ? selectedNode.queue.some((entry) => !holdsOn(selectedNode.name).some((hold) => hold.job === entry.job)) : false,
  );

  function onNodeChange(value: string): void {
    nodeChoice = value;
    syncFamily();
  }

  function grabSummary(result: RgGrabResponse, queuedAfter: string | null): string {
    const where = result.node === NEXT_GH200 ? " on the next available GH200" : result.node ? ` on ${result.node}` : "";
    const what = result.profile ? ` serving ${result.profile}` : ", hold only";
    switch (result.status) {
      case "placed":
        if (queuedAfter) {
          const plan = result.profile ? `, to serve ${result.profile}` : ", hold only";
          const boot = result.profile ? ", then the model boots" : "";
          return `Hold ${result.job} queued after ${queuedAfter}${where}${plan}. It can start once ${queuedAfter} ends${boot}.`;
        }
        return `Hold ${result.job} placed${where}${what}. It starts when Slurm schedules it.`;
      case "existing":
        return `Hold ${result.job} already exists${where}.`;
      case "unverified":
        return `Hold ${result.job} was submitted but not verified${where}. Check it in Your holds before relying on it.`;
      default:
        return result.detail ?? result.reason ?? result.status;
    }
  }

  async function submitGrab(event: SubmitEvent): Promise<void> {
    event.preventDefault();
    if (durationError || grabbing) return;
    grabbing = true;
    grabResult = null;
    try {
      const follow = followJob;
      const result = await grabRg({ node: nodeChoice, family, duration: canonicalDuration(duration), caller: "page", mode: "page", ...(follow ? { follow } : {}) });
      grabResult = { ok: result.status !== "unverified", text: grabSummary(result, follow) };
    } catch (cause) {
      grabResult = { ok: false, text: rgActionErrorText(cause, "Grab failed") };
    } finally {
      grabbing = false;
      // Refetch on failure too, because a timed-out grab may still have placed a hold.
      await load();
    }
  }

  // ---- Your holds ---------------------------------------------------------

  let releaseTarget = $state<RgHold | null>(null);
  let releasing = $state(false);
  let releaseError = $state("");
  // rg-api's refusal reason for the last attempt; it decides whether "Force release" is offered
  let releaseErrorCode = $state<string | null>(null);

  function holdsOn(name: string): RgHold[] {
    return overview?.holds.filter((h) => h.node === name) ?? [];
  }

  function servingLine(hold: RgHold): string {
    if (!hold.serving) return "Hold only";
    return `Serving ${hold.serving.profile} · ${hold.serving.state}`;
  }

  function askRelease(hold: RgHold): void {
    releaseError = "";
    releaseErrorCode = null;
    releaseTarget = hold;
  }

  // force is only ever true from the Force release button, which appears after a refused release (D21)
  async function confirmRelease(force = false): Promise<void> {
    if (!releaseTarget || releasing) return;
    releasing = true;
    releaseError = "";
    releaseErrorCode = null;
    try {
      await releaseRg(releaseTarget.job, force);
      releaseTarget = null;
    } catch (cause) {
      releaseError = rgActionErrorText(cause, "Release failed");
      releaseErrorCode = cause instanceof RgApiError ? cause.code : null;
    } finally {
      releasing = false;
      // Refetch on failure too, because a timed-out release may still have completed.
      await load();
    }
  }
</script>

<div class="p-2">
  <div class="mt-4 mb-4 flex items-start justify-between gap-3">
    <div>
      <h3 class="text-lg font-semibold">RG GPUs</h3>
      <p class="text-sm text-muted-foreground">
        {#if overview}Updated {formatClock(overview.generated_at)}.{:else}Rogues Gallery GPUs on demand.{/if}
      </p>
    </div>
    <Button variant="outline" size="sm" onclick={() => void load()} disabled={loading} aria-label="Refresh">
      <RefreshCw class={loading ? "animate-spin" : ""} />
      Refresh
    </Button>
  </div>

  {#if error && !overview}
    <div class="rounded-lg border border-destructive/50 p-6">
      <h4 class="font-semibold">{errorTitle}</h4>
      {#if errorHint}<p class="mt-1 text-sm text-muted-foreground">{errorHint}</p>{/if}
      {#if error.message}<p class="mt-1 font-mono text-xs text-muted-foreground">{error.message}</p>{/if}
    </div>
  {:else if !overview}
    <div class="rounded-lg border p-6 text-sm text-muted-foreground">Loading RG GPUs…</div>
  {:else}
    {#if error}
      <div class="mb-4 rounded-lg border border-destructive/50 p-3 text-sm">
        <span class="font-semibold">{errorTitle}.</span> Showing the last data. <span class="font-mono text-xs">{error.message}</span>
      </div>
    {/if}
    {#if overview.errors.length > 0}
      <div class="mb-4 rounded-lg border border-amber-500/50 p-3 text-sm">
        <span class="font-semibold">Some sources failed:</span>
        {overview.errors.join("; ")}
      </div>
    {/if}

    <div class="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
      {#each overview.nodes as node (node.name)}
        <RgNodeCard {node} reference={overview.reference} holds={holdsOn(node.name)} generatedAt={overview.generated_at} {elapsedS} />
      {/each}
    </div>

    <h4 class="mt-6 mb-2 text-sm font-semibold text-muted-foreground">Your holds</h4>
    {#if overview.holds.length === 0}
      <div class="rounded-lg border p-4 text-sm text-muted-foreground">
        {#if holdsUnavailable(overview.errors)}
          Hold status unavailable (rg-hold status failed); holds may exist that are not listed.
        {:else}
          No holds. Grab a GPU below.
        {/if}
      </div>
    {:else}
      <div class="space-y-2">
        {#each overview.holds as hold (hold.job)}
          <div class="flex flex-wrap items-center justify-between gap-2 rounded-lg border p-3">
            <div class="min-w-0 text-sm">
              <div class="flex flex-wrap items-center gap-2">
                <span class="font-semibold">{hold.node}</span>
                <span class="font-mono text-xs text-muted-foreground">job {hold.job}</span>
                {#if idleBadgeShown(hold)}<Badge variant="destructive">Idle</Badge>{/if}
              </div>
              <div class="text-muted-foreground">{holdStatus(hold, elapsedS, busyNow)}</div>
              <div class="text-muted-foreground">{servingLine(hold)}</div>
              {#if hold.serving?.error}<div class="text-destructive text-xs">{hold.serving.error}</div>{/if}
            </div>
            <Button variant="destructive" size="sm" onclick={() => askRelease(hold)}>Release</Button>
          </div>
        {/each}
      </div>
    {/if}

    <h4 class="mt-6 mb-2 text-sm font-semibold text-muted-foreground">Grab a GPU</h4>
    <Card.Root size="sm" class="max-w-md">
      <Card.Content>
        <form class="space-y-3" onsubmit={submitGrab}>
          <div class="space-y-1">
            <label class="text-xs text-muted-foreground" for="rg-node">GPU</label>
            <Select.Root type="single" value={nodeChoice} onValueChange={(v) => v && onNodeChange(v)}>
              <Select.Trigger id="rg-node" class="w-full">{nodeLabel}</Select.Trigger>
              <Select.Content>
                <Select.Item value={BEST}>{bestLabel()}</Select.Item>
                <Select.Item value={NEXT_GH200}>Next available GH200</Select.Item>
                {#each overview.nodes as node (node.name)}
                  <Select.Item value={node.name}>{node.name} · {node.gpu} · {node.availability}</Select.Item>
                {/each}
              </Select.Content>
            </Select.Root>
          </div>

          <div class="space-y-1">
            <label class="text-xs text-muted-foreground" for="rg-family">Serve</label>
            <Select.Root type="single" value={family} onValueChange={(v) => v && (family = v as RgFamilyChoice)}>
              <Select.Trigger id="rg-family" class="w-full">{familyLabel(family)}</Select.Trigger>
              <Select.Content>
                {#each families as option (option.value)}
                  <Select.Item value={option.value}>{option.label}</Select.Item>
                {/each}
              </Select.Content>
            </Select.Root>
          </div>

          <div class="space-y-1">
            <label class="text-xs text-muted-foreground" for="rg-duration">Duration</label>
            <Input id="rg-duration" bind:value={duration} autocomplete="off" aria-invalid={durationError !== ""} />
            <p class="text-xs {durationError ? 'text-destructive' : 'text-muted-foreground'}">
              {durationError || `2h, 90m or 1h30m. Up to ${maxTimeLabel}.`}
            </p>
          </div>

          {#if nodeChoice === NEXT_GH200}
            <p class="text-xs text-muted-foreground">Queues one hold for either GH200. Slurm assigns the node when it starts{family === "hold-only" ? "." : ", then the model boots."}</p>
          {/if}
          {#if followJob}
            <p class="text-xs text-muted-foreground">
              You have hold {followJob} on {nodeChoice}, so this queues a new hold that can start once it ends{family === "hold-only" ? "" : ", then the model boots under it"}.
              {#if othersQueued}Someone else already queued for {nodeChoice} may start first.{/if}
            </p>
          {/if}
          <Button type="submit" class="w-full" disabled={grabbing || durationError !== ""}>
            {grabbing ? (followJob ? "Queueing…" : "Grabbing…") : followJob ? `Queue after ${followJob}` : "Grab"}
          </Button>

          {#if grabResult}
            <p class="text-sm {grabResult.ok ? '' : 'text-destructive'}">{grabResult.text}</p>
          {/if}
        </form>
      </Card.Content>
    </Card.Root>
  {/if}
</div>

<!-- A function binding, not open={...} plus onOpenChange, because bits-ui writes
     `open` locally when the parent has no setter, so a dismiss during a release
     would close the dialog and hide a failed release. -->
<Dialog.Root
  bind:open={
    () => releaseTarget !== null,
    (open) => {
      if (!open && !releasing) releaseTarget = null;
    }
  }
>
  <Dialog.Content
    escapeKeydownBehavior={releasing ? "ignore" : "close"}
    interactOutsideBehavior={releasing ? "ignore" : "close"}
    showCloseButton={!releasing}
  >
    {#if releaseTarget}
      <Dialog.Header>
        <Dialog.Title>Release hold {releaseTarget.job}?</Dialog.Title>
        <Dialog.Description>
          This cancels the Slurm job on {releaseTarget.node}{timeLeftText(releaseTarget.time_left_s, elapsedS)
            ? ` (${timeLeftText(releaseTarget.time_left_s, elapsedS)})`
            : ""}{releaseTarget.serving ? " and stops the model serving on it" : ""}.
        </Dialog.Description>
      </Dialog.Header>
      {#if releaseError}<p class="text-destructive text-sm">{releaseError}</p>{/if}
      {#if forceReleaseOffered(releaseErrorCode)}
        <p class="text-sm text-muted-foreground">
          The normal release was refused. Force release cancels every step of job {releaseTarget.job}, removes the model
          container through a fresh step, confirms it is gone, then cancels the hold. Use it only for a hung hold.
        </p>
      {/if}
      <Dialog.Footer>
        <Button variant="outline" onclick={() => (releaseTarget = null)} disabled={releasing}>Keep</Button>
        <Button variant="destructive" onclick={() => void confirmRelease()} disabled={releasing}>
          {releasing ? "Releasing…" : "Release"}
        </Button>
        {#if forceReleaseOffered(releaseErrorCode)}
          <Button variant="destructive" onclick={() => void confirmRelease(true)} disabled={releasing}>
            Force release
          </Button>
        {/if}
      </Dialog.Footer>
    {/if}
  </Dialog.Content>
</Dialog.Root>
