<script lang="ts">
  import Tag from "../Tag.svelte";

  interface Props {
    status: string;
    title?: string;
  }

  let { status, title }: Props = $props();

  // Production carries the success token because it's the row people compare
  // against; rollback is amber like the page's other "watch this" notices.
  const tones: Record<string, string> = {
    production: "bg-success/15 text-success",
    rollback: "bg-amber-500/15 text-amber-700 dark:text-amber-400",
    "a-b": "bg-primary/15 text-primary",
    retired: "opacity-70",
  };
  const labels: Record<string, string> = { "a-b": "A/B" };
</script>

<span {title}><Tag class="whitespace-nowrap {tones[status] ?? ''}">{labels[status] ?? status}</Tag></span>
