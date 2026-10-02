<script lang="ts">
  import Tag from "../Tag.svelte";

  interface Props {
    status: string;
    title?: string;
  }

  let { status, title }: Props = $props();

  // Production carries the success hue because it's the row people compare
  // against; rollback is amber like the page's other "watch this" notices.
  // Text shades are picked for APCA Lc >= 60 on their tint (12 px labels),
  // because the token-on-own-tint pairs measured Lc 51.5 / -52 (success) and
  // 33-55 (primary, per theme). Measured 2026-10-01 from index.css tokens:
  //   production teal-800 / emerald-300 on success/15: 73.6 / -77.3
  //   rollback amber-700 / amber-400 on amber-500/15:  66.0 / -69.8
  //   a-b foreground on primary/15: 87-97 / -96..-103 (mc light 71)
  //   neutral muted-foreground / foreground/75 on muted: 66.9 / -68.4
  const tones: Record<string, string> = {
    production: "bg-success/15 text-teal-800 dark:text-emerald-300",
    rollback: "bg-amber-500/15 text-amber-700 dark:text-amber-400",
    "a-b": "bg-primary/15 text-foreground",
    registered: "dark:text-foreground/75",
    retired: "dark:text-foreground/75",
    superseded: "line-through dark:text-foreground/75",
  };
  const labels: Record<string, string> = { "a-b": "A/B" };
</script>

<span {title}><Tag class="whitespace-nowrap {tones[status] ?? ''}">{labels[status] ?? status}</Tag></span>
