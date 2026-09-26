<script lang="ts">
  import { hasListedModels } from "../../stores/api";
  import { createPlaygroundInterface } from "../../lib/playgroundInterface";
  import { transcribeAudio } from "../../lib/audioApi";
  import { playgroundStores } from "../../stores/playgroundActivity";
  import ModelSelector from "./ModelSelector.svelte";
  import EmptyState from "../EmptyState.svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Copy, Check } from "@lucide/svelte";
  import { formatFileSize } from "../../lib/format";
  import { copyText } from "../../lib/clipboard";

  const iface = createPlaygroundInterface("playground-audio-model", playgroundStores.audioTranscribing);
  const selectedModelStore = iface.selectedModel;
  const transcribing = iface.busy;
  const error = iface.error;
  let isTranscribing = $derived($transcribing);

  let selectedFile = $state<File | null>(null);
  let transcriptionResult = $state<string | null>(null);
  let isDragging = $state(false);
  let fileInput = $state<HTMLInputElement | null>(null);
  let copied = $state(false);

  // Broad on purpose: our ASR backends decode every upload with ffmpeg, so anything with an audio track
  // works, video included. The extension list covers files whose browser MIME type comes back empty
  // or generic (common for .opus, .m4b, .mkv); an undecodable file comes back as a server error, and a
  // file with no audio track as an empty result (shown as "No speech detected").
  const ACCEPTED_EXTENSIONS = [
    '.mp3', '.wav', '.ogg', '.oga', '.opus', '.flac', '.m4a', '.m4b', '.aac', '.wma', '.aiff', '.aif',
    '.amr', '.webm', '.mp4', '.m4v', '.mov', '.mkv', '.avi',
  ];
  const ACCEPT_ATTR = ['audio/*', 'video/*', ...ACCEPTED_EXTENSIONS].join(',');

  let canTranscribe = $derived(selectedFile !== null && $selectedModelStore !== "" && !isTranscribing);

  function validateFile(file: File): { valid: boolean; error?: string } {
    const ext = '.' + file.name.split('.').pop()?.toLowerCase();
    const isMedia = file.type.startsWith('audio/') || file.type.startsWith('video/');

    if (!isMedia && !ACCEPTED_EXTENSIONS.includes(ext)) {
      return { valid: false, error: 'Not an audio or video file' };
    }

    return { valid: true };
  }

  function selectFile(file: File) {
    const validation = validateFile(file);
    if (validation.valid) {
      selectedFile = file;
      $error = null;
      transcriptionResult = null;
    } else {
      $error = validation.error || "Invalid file";
      selectedFile = null;
    }
  }

  function handleFileSelect(event: Event) {
    const target = event.target as HTMLInputElement;
    const file = target.files?.[0];
    if (file) selectFile(file);
  }

  // The whole tab is the drop target in every state (file chosen, result, error) so a new file can
  // replace the old one, and preventDefault stops the browser from navigating away to open the file —
  // which would also kill a running transcription. Drops are ignored mid-transcription because the
  // running request still owns the selected file. Non-file drags (selected text, links) are left alone.
  function handleDragOver(event: DragEvent) {
    if (!event.dataTransfer?.types.includes("Files")) return;
    event.preventDefault();
    if (isTranscribing) {
      if (event.dataTransfer) event.dataTransfer.dropEffect = "none";
      return;
    }
    isDragging = true;
  }

  function handleDragLeave(event: DragEvent) {
    // dragleave also fires when the pointer crosses into a child element; only clear the highlight
    // when it actually leaves the tab, or it flickers while moving over the controls.
    const root = event.currentTarget as HTMLElement;
    if (!root.contains(event.relatedTarget as Node | null)) isDragging = false;
  }

  function handleDrop(event: DragEvent) {
    if (!event.dataTransfer?.types.includes("Files")) return;
    event.preventDefault();
    isDragging = false;
    if (isTranscribing) return;

    const file = event.dataTransfer?.files[0];
    if (file) selectFile(file);
  }

  async function transcribe() {
    const file = selectedFile;
    if (!file || !$selectedModelStore || isTranscribing) return;

    transcriptionResult = null;
    await iface.run(async (signal) => {
      const response = await transcribeAudio($selectedModelStore, file, signal);
      transcriptionResult = response.text;
    });
  }

  function cancelTranscription() {
    iface.cancel();
  }

  function clearAll() {
    selectedFile = null;
    transcriptionResult = null;
    $error = null;
    if (fileInput) {
      fileInput.value = '';
    }
  }

  async function copyToClipboard() {
    if (transcriptionResult && (await copyText(transcriptionResult))) {
      copied = true;
      setTimeout(() => {
        copied = false;
      }, 2000);
    }
  }
</script>

<div
  role="region"
  aria-label="Audio transcription (drop an audio file anywhere here)"
  class="flex flex-col h-full"
  ondragover={handleDragOver}
  ondragleave={handleDragLeave}
  ondrop={handleDrop}
>
  <!-- Model selector -->
  <div class="shrink-0 flex flex-wrap gap-2 mb-4">
    <ModelSelector
      bind:value={$selectedModelStore}
      placeholder="Select an audio model..."
      disabled={isTranscribing}
      match={{ inputModalities: ["audio"], outputModalities: ["text"] }}
    />
  </div>

  <!-- Empty state for no models configured -->
  {#if !$hasListedModels}
    <EmptyState message="No models configured. Add models to your configuration to transcribe audio." />
  {:else}
    <!-- File upload / Result display area -->
    <div
      class="flex-1 overflow-auto mb-4 flex items-center justify-center rounded-md border-2 border-dashed transition-colors {isDragging ? 'border-primary bg-primary/10' : 'border-border bg-background'}"
    >
      {#if isDragging}
        <p class="text-primary font-medium pointer-events-none">
          {selectedFile || transcriptionResult !== null ? "Drop to replace the current file" : "Drop audio file to select it"}
        </p>
      {:else if isTranscribing}
        <div class="text-center text-muted-foreground">
          <div class="inline-block w-8 h-8 border-4 border-primary border-t-transparent rounded-full animate-spin mb-2"></div>
          <p>Transcribing audio...</p>
        </div>
      {:else if $error}
        <div class="text-center text-red-500 p-4">
          <p class="font-medium">Error</p>
          <p class="text-sm mt-1">{$error}</p>
        </div>
      {:else if transcriptionResult !== null}
        <div class="w-full h-full flex flex-col p-4">
          <div class="flex justify-between items-center mb-2">
            <h3 class="pb-0 font-medium">Transcription Result</h3>
            <Button
              variant="outline"
              size="icon-sm"
              onclick={copyToClipboard}
              title={copied ? 'Copied!' : 'Copy to clipboard'}
            >
              {#if copied}
                <Check class="text-success" />
              {:else}
                <Copy />
              {/if}
            </Button>
          </div>
          <div class="flex-1 overflow-auto p-3 rounded-md border border-border bg-background whitespace-pre-wrap">
            {#if transcriptionResult}
              {transcriptionResult}
            {:else}
              <span class="text-muted-foreground italic">No speech detected</span>
            {/if}
          </div>
        </div>
      {:else if selectedFile}
        <div class="text-center text-muted-foreground p-4">
          <p class="font-medium mb-2">File Selected</p>
          <p class="text-sm">{selectedFile.name}</p>
          <p class="text-xs mt-1">{formatFileSize(selectedFile.size)}</p>
        </div>
      {:else}
        <button
          type="button"
          class="w-full h-full flex items-center justify-center text-center text-muted-foreground p-8 cursor-pointer hover:bg-muted/40"
          onclick={() => fileInput?.click()}
        >
          <span class="block">
            <span class="block mb-2">Drag and drop an audio file here</span>
            <span class="block text-sm">or click to browse</span>
            <span class="block text-xs mt-4">Any audio or video file (MP3, WAV, OGG, M4A, FLAC, OPUS, MP4, …)</span>
          </span>
        </button>
      {/if}
    </div>

    <!-- File input and transcribe button -->
    <div class="shrink-0 flex gap-2">
      <input
        type="file"
        accept={ACCEPT_ATTR}
        class="hidden"
        onchange={handleFileSelect}
        bind:this={fileInput}
      />
      <Button variant="outline" onclick={() => fileInput?.click()} disabled={isTranscribing}>
        Browse Files
      </Button>
      <div class="flex-1"></div>
      {#if isTranscribing}
        <Button variant="destructive" onclick={cancelTranscription}>Cancel</Button>
      {:else}
        <Button onclick={transcribe} disabled={!canTranscribe}>Transcribe</Button>
        <Button
          variant="outline"
          onclick={clearAll}
          disabled={!selectedFile && !transcriptionResult && !$error}
        >
          Clear
        </Button>
      {/if}
    </div>
  {/if}
</div>
