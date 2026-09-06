<script lang="ts">
  import type { Model } from "../lib/types";
  import { pendingLoads, onToggleLoad } from "../stores/modelLoad";
  import { Play, PowerOff, Loader2 } from "@lucide/svelte";

  interface Props {
    model: Model;
    /** "md" for list rows (size-7), "sm" for the detail header (size-5). */
    size?: "md" | "sm";
  }

  let { model, size = "md" }: Props = $props();

  let btnSize = $derived(size === "sm" ? "size-5 rounded-sm" : "size-7 rounded-md");
  let iconSize = $derived(size === "sm" ? "size-3.5" : "size-4");
  // A load in progress (starting, or a just-fired request still pending) stays
  // clickable so the operator can cancel it — cancelling a multi-minute remote
  // load is the whole point. Only an unload in progress (stopping) is inert.
  let loading = $derived(model.state === "starting" || ($pendingLoads[model.id] && model.state === "stopped"));
  let stopping = $derived(model.state === "stopping");
</script>

<button
  type="button"
  class="text-muted-foreground hover:bg-accent hover:text-accent-foreground flex {btnSize} shrink-0 items-center justify-center disabled:opacity-50"
  title={model.state === "ready" ? "Unload" : loading ? "Cancel" : "Load"}
  aria-label={model.state === "ready" ? "Unload model" : loading ? "Cancel load" : "Load model"}
  disabled={stopping}
  onclick={() => onToggleLoad(model)}
>
  {#if loading}
    <Loader2 class="{iconSize} animate-spin" />
  {:else if model.state === "ready"}
    <PowerOff class={iconSize} />
  {:else if stopping}
    <Loader2 class="{iconSize} animate-spin" />
  {:else}
    <Play class={iconSize} />
  {/if}
</button>
