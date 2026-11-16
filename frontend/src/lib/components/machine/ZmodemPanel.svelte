<script>
  /**
   * ZmodemPanel — the request dialog and the status strip for a
   * terminal's ZMODEM transfer.
   *
   * Everything is presentation: the state, the frames and the byte
   * counters belong to the ZmodemClient the terminal owns, so the two
   * halves can sit in different corners of the panel and still agree.
   *
   * The dialog has two directions because a telnet or serial link has two
   * problems with no other answer: pulling a file the remote will part with
   * (sz) and pushing one it will accept (rz).
   */
  import { t } from '$lib/i18n.svelte.js';
  import { Button } from '$lib/components/ui/button';
  import { Label } from '$lib/components/ui/label';
  import {
    Dialog as DialogRoot,
    DialogContent,
    DialogDescription,
    DialogFooter,
    DialogHeader,
    DialogTitle,
  } from '$lib/components/ui/dialog';
  import { Loader2, CircleAlert, CircleCheck, X, Ban, Upload, Trash2 } from '@lucide/svelte';
  import { formatBytes } from '$lib/utils/format.js';
  import { flattenDrop } from '$lib/utils/fileTransfer.js';

  let { client } = $props();

  const lines = $derived(client.paths.length);
  const up = $derived(client.mode === 'up');
  const bytes = $derived(client.pickedBytes);
  let fileInput = $state();

  function onkey(e) {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) client.start();
  }

  function onPick(e) {
    client.pick(e.target.files);
    // So the same file chosen twice still fires the change event.
    e.target.value = '';
  }

  async function onDrop(e) {
    e.preventDefault();
    const items = await flattenDrop(e.dataTransfer);
    client.pick(items.filter((it) => it.file));
  }
</script>

<DialogRoot bind:open={client.open}>
  <DialogContent class="sm:max-w-lg [&>*]:min-w-0">
    <DialogHeader>
      <DialogTitle>{t(up ? 'terminal.zmodemTitleUp' : 'terminal.zmodemTitle')}</DialogTitle>
      <DialogDescription
        >{t(up ? 'terminal.zmodemUploadHint' : 'terminal.zmodemHint')}</DialogDescription
      >
    </DialogHeader>

    <div class="inline-flex rounded-md border border-border p-0.5 text-xs">
      <button
        type="button"
        class="rounded px-3 py-1 {!up
          ? 'bg-muted font-medium text-foreground'
          : 'text-muted-foreground'}"
        aria-pressed={!up}
        onclick={() => client.setMode('down')}
      >
        {t('terminal.zmodemTabDown')}
      </button>
      <button
        type="button"
        class="rounded px-3 py-1 {up
          ? 'bg-muted font-medium text-foreground'
          : 'text-muted-foreground'}"
        aria-pressed={up}
        onclick={() => client.setMode('up')}
      >
        {t('terminal.zmodemTabUp')}
      </button>
    </div>

    {#if up}
      <div
        role="group"
        aria-label={t('terminal.zmodemTabUp')}
        class="space-y-2 rounded-md border border-dashed border-border p-3"
        ondrop={onDrop}
        ondragover={(e) => e.preventDefault()}
      >
        <input
          id="zmodem-files"
          type="file"
          multiple
          class="hidden"
          bind:this={fileInput}
          onchange={onPick}
        />
        <div class="flex items-center gap-2">
          <Button variant="outline" size="sm" onclick={() => fileInput?.click()}>
            <Upload class="w-3.5 h-3.5" />
            {t('terminal.zmodemChooseFiles')}
          </Button>
          {#if client.picked.length}
            <button
              type="button"
              class="flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
              onclick={() => client.clearPicked()}
            >
              <Trash2 class="w-3.5 h-3.5" />
              {t('terminal.zmodemClear')}
            </button>
          {/if}
        </div>
        {#if client.picked.length}
          <ul class="max-h-40 overflow-auto rounded border border-border divide-y divide-border">
            {#each client.picked as f (f.name + f.size)}
              <li class="flex items-center justify-between gap-3 px-2 py-1 text-xs">
                <span class="truncate" title={f.name}>{f.name}</span>
                <span class="font-mono text-[11px] text-muted-foreground shrink-0">
                  {formatBytes(f.size)}
                </span>
              </li>
            {/each}
          </ul>
          <p class="text-[11px] text-muted-foreground">
            {t('terminal.zmodemStaged', { count: client.picked.length, size: formatBytes(bytes) })}
          </p>
        {:else}
          <p class="text-[11px] text-muted-foreground">{t('terminal.zmodemNoFiles')}</p>
        {/if}
      </div>
    {:else}
      <div class="space-y-2">
        <Label for="zmodem-paths">{t('terminal.zmodemPaths')}</Label>
        <textarea
          id="zmodem-paths"
          bind:value={client.input}
          onkeydown={onkey}
          rows="4"
          spellcheck="false"
          autocomplete="off"
          class="w-full rounded-md border border-border bg-background p-2 font-mono text-xs text-foreground outline-none focus:ring-2 focus:ring-primary/40"
          placeholder={'/var/log/messages\n/etc/config'}
        ></textarea>
        <p class="text-[11px] text-muted-foreground">
          {lines > 0 ? t('terminal.zmodemCount', { count: lines }) : t('terminal.zmodemOnePerLine')}
        </p>
        <label class="flex items-center gap-2 text-xs text-muted-foreground select-none">
          <input type="checkbox" bind:checked={client.recursive} class="accent-primary" />
          {t('terminal.zmodemRecursive')}
        </label>
      </div>
    {/if}

    {#if client.error}
      <div class="text-xs text-destructive">{client.error}</div>
    {/if}

    <DialogFooter class="gap-2">
      <Button variant="outline" onclick={() => (client.open = false)}>{t('common.cancel')}</Button>
      <Button
        onclick={() => client.start()}
        disabled={up ? client.picked.length === 0 : lines === 0}
      >
        {t(up ? 'terminal.zmodemSend' : 'terminal.zmodemStart')}
      </Button>
    </DialogFooter>
  </DialogContent>
</DialogRoot>

{#if client.kind}
  <div
    class="flex items-center gap-2 px-3 py-1.5 text-xs border-b {client.kind === 'error'
      ? 'bg-destructive/10 border-destructive/40'
      : client.kind === 'done'
        ? 'bg-emerald-500/10 border-emerald-500/40'
        : 'bg-amber-500/10 border-amber-500/40'}"
    role="status"
    aria-live="polite"
  >
    {#if client.kind === 'running'}
      <Loader2 class="w-3.5 h-3.5 shrink-0 animate-spin text-primary" />
    {:else if client.kind === 'done'}
      <CircleCheck class="w-3.5 h-3.5 shrink-0 text-emerald-500" />
    {:else}
      <CircleAlert class="w-3.5 h-3.5 shrink-0 text-destructive" />
    {/if}
    <span class="flex-1 min-w-0 truncate" title={client.notice}>{client.notice}</span>
    {#if client.running && client.staged > 0 && client.staged < 100}
      <span class="font-mono text-[11px] text-muted-foreground shrink-0">{client.staged}%</span>
    {/if}
    {#if client.running && (client.sizeLabel || client.currentLabel)}
      <span class="font-mono text-[11px] text-muted-foreground shrink-0 max-w-[45%] truncate">
        {[client.fileLabel, client.currentLabel, client.sizeLabel].filter(Boolean).join(' · ')}
      </span>
    {/if}
    {#if client.running}
      <button
        type="button"
        onclick={() => client.cancel()}
        class="flex items-center gap-1 px-2 py-0.5 rounded border border-border hover:bg-muted"
      >
        <Ban class="w-3 h-3" />
        {t('common.cancel')}
      </button>
    {/if}
    <button
      type="button"
      onclick={() => client.dismiss()}
      class="p-0.5 rounded border border-transparent hover:border-border"
      aria-label={t('terminal.guestCopyDismiss')}
      title={t('terminal.guestCopyDismiss')}
    >
      <X class="w-3.5 h-3.5" />
    </button>
  </div>
{/if}
