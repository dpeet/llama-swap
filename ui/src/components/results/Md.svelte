<script lang="ts">
  // The small markdown subset the results notes use (`code`, **bold**, "- "
  // bullets), rendered as text nodes, so catalog text never reaches {@html}.
  interface Props {
    text: string;
    class?: string;
    /** One line as a <span> (code and bold only), for labels inside buttons and headings. */
    inline?: boolean;
  }

  let { text, class: className = "", inline: inlineOnly = false }: Props = $props();

  type Segment = { kind: "text" | "code" | "bold"; text: string };
  type Block = { kind: "p"; segments: Segment[] } | { kind: "ul"; items: Segment[][] };

  function segments(line: string): Segment[] {
    return line
      .split(/(`[^`]+`|\*\*[^*]+\*\*)/)
      .filter((part) => part !== "")
      .map((part) =>
        part.startsWith("`") && part.endsWith("`") && part.length > 1
          ? { kind: "code", text: part.slice(1, -1) }
          : part.startsWith("**") && part.endsWith("**") && part.length > 3
            ? { kind: "bold", text: part.slice(2, -2) }
            : { kind: "text", text: part },
      );
  }

  let blocks = $derived.by(() => {
    const out: Block[] = [];
    for (const line of text.split("\n")) {
      if (line.trim() === "") continue;
      const bullet = /^\s*[-*] (.*)$/.exec(line);
      const last = out[out.length - 1];
      if (bullet) {
        if (last?.kind === "ul") last.items.push(segments(bullet[1]));
        else out.push({ kind: "ul", items: [segments(bullet[1])] });
      } else {
        out.push({ kind: "p", segments: segments(line) });
      }
    }
    return out;
  });
</script>

{#snippet inline(parts: Segment[])}
  {#each parts as part, i (i)}
    {#if part.kind === "code"}<code class="bg-muted rounded px-1 font-mono text-[0.85em] break-all">{part.text}</code>{:else if part.kind === "bold"}<strong class="font-semibold">{part.text}</strong>{:else}{part.text}{/if}
  {/each}
{/snippet}

{#if inlineOnly}
  <span class={className}>{@render inline(segments(text.replaceAll("\n", " ")))}</span>
{:else}
  <div class="space-y-1.5 {className}">
    {#each blocks as block, i (i)}
      {#if block.kind === "ul"}
        <ul class="list-disc space-y-1 pl-5">
          {#each block.items as item, j (j)}
            <li>{@render inline(item)}</li>
          {/each}
        </ul>
      {:else}
        <p>{@render inline(block.segments)}</p>
      {/if}
    {/each}
  </div>
{/if}
