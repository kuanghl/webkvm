<script>
  /**
   * EntryDialogs — new folder, rename, chmod and preview.
   *
   * The dialog state lives in the pane (one object, four booleans) so a
   * FilePane can open the right dialog from a row action, a toolbar
   * button or the keyboard without five parallel prop wires.
   */
  import { t } from '$lib/i18n.svelte.js';
  import { Button } from '$lib/components/ui/button';
  import { Input } from '$lib/components/ui/input';
  import { Label } from '$lib/components/ui/label';
  import {
    Dialog as DialogRoot,
    DialogContent,
    DialogHeader,
    DialogTitle,
    DialogDescription,
    DialogFooter,
  } from '$lib/components/ui/dialog';
  import Alert from '$lib/components/Alert.svelte';
  import Spinner from '$lib/components/Spinner.svelte';
  import { Loader2, Plus } from '@lucide/svelte';
  import { chmodEntry, makeFolder, renameEntry } from './fileActions.js';
  import { messageOf } from './paneModel.svelte.js';

  let { pane, dlg, ondone = () => {} } = $props();

  const OPENER = { mkdir: 'mkdir', rename: 'rename', chmod: 'chmod' };

  async function run(kind) {
    if (dlg.busy) return;
    dlg.busy = true;
    dlg.error = '';
    try {
      if (kind === 'mkdir') await makeFolder(pane, dlg.value.trim());
      else if (kind === 'rename') await renameEntry(pane, dlg.entry, dlg.value.trim());
      else if (kind === 'chmod') await chmodEntry(pane, dlg.entry, dlg.value.trim());
      dlg[OPENER[kind]] = false;
      dlg.entry = null;
      ondone();
    } catch (e) {
      dlg.error = messageOf(e);
    } finally {
      dlg.busy = false;
    }
  }
</script>

<!-- New folder -->
<DialogRoot bind:open={dlg.mkdir}>
  <DialogContent class="sm:max-w-md [&>*]:min-w-0">
    <DialogHeader>
      <DialogTitle>{t('files.newFolder')}</DialogTitle>
      <DialogDescription class="font-mono text-xs break-all">{pane.path}</DialogDescription>
    </DialogHeader>
    <div class="space-y-2">
      <Label for="nf-{pane.side}">{t('files.name')}</Label>
      <Input
        id="nf-{pane.side}"
        bind:value={dlg.value}
        class="w-full font-mono"
        onkeydown={(e) => e.key === 'Enter' && run('mkdir')}
      />
      {#if dlg.error}
        <div class="text-xs text-destructive">{dlg.error}</div>
      {/if}
    </div>
    <DialogFooter class="gap-2">
      <Button variant="outline" onclick={() => (dlg.mkdir = false)} disabled={dlg.busy}
        >{t('common.cancel')}</Button
      >
      <Button onclick={() => run('mkdir')} disabled={dlg.busy}>
        {#if dlg.busy}
          <Loader2 class="h-3.5 w-3.5 mr-1.5 animate-spin" />
        {:else}
          <Plus class="h-3.5 w-3.5 mr-1.5" />
        {/if}
        {t('common.create')}
      </Button>
    </DialogFooter>
  </DialogContent>
</DialogRoot>

<!-- Rename -->
<DialogRoot bind:open={dlg.rename}>
  <DialogContent class="sm:max-w-md [&>*]:min-w-0">
    <DialogHeader>
      <DialogTitle>{t('files.rename')}</DialogTitle>
      <DialogDescription class="font-mono text-xs break-all">{dlg.entry?.path}</DialogDescription>
    </DialogHeader>
    <div class="space-y-2">
      <Label for="rn-{pane.side}">{t('files.renameTo')}</Label>
      <Input
        id="rn-{pane.side}"
        bind:value={dlg.value}
        class="w-full font-mono"
        onkeydown={(e) => e.key === 'Enter' && run('rename')}
      />
      {#if dlg.error}
        <div class="text-xs text-destructive">{dlg.error}</div>
      {/if}
    </div>
    <DialogFooter class="gap-2">
      <Button variant="outline" onclick={() => (dlg.rename = false)} disabled={dlg.busy}
        >{t('common.cancel')}</Button
      >
      <Button onclick={() => run('rename')} disabled={dlg.busy}>{t('common.save')}</Button>
    </DialogFooter>
  </DialogContent>
</DialogRoot>

<!-- Chmod -->
<DialogRoot bind:open={dlg.chmod}>
  <DialogContent class="sm:max-w-md [&>*]:min-w-0">
    <DialogHeader>
      <DialogTitle>{t('files.chmod')}</DialogTitle>
      <DialogDescription class="font-mono text-xs break-all">{dlg.entry?.path}</DialogDescription>
    </DialogHeader>
    <div class="space-y-2">
      <Label for="cm-{pane.side}">{t('files.permissions')}</Label>
      <Input
        id="cm-{pane.side}"
        bind:value={dlg.value}
        placeholder={t('machines.sftpModePlaceholder')}
        class="w-full font-mono"
        onkeydown={(e) => e.key === 'Enter' && run('chmod')}
      />
      <p class="text-[11px] text-muted-foreground">{t('files.chmodHint')}</p>
      {#if dlg.error}
        <div class="text-xs text-destructive">{dlg.error}</div>
      {/if}
    </div>
    <DialogFooter class="gap-2">
      <Button variant="outline" onclick={() => (dlg.chmod = false)} disabled={dlg.busy}
        >{t('common.cancel')}</Button
      >
      <Button onclick={() => run('chmod')} disabled={dlg.busy}>{t('common.save')}</Button>
    </DialogFooter>
  </DialogContent>
</DialogRoot>

<!-- Preview: the pane fills dlg.text before opening, so this stays presentational. -->
<DialogRoot bind:open={dlg.preview}>
  <DialogContent class="sm:max-w-2xl [&>*]:min-w-0">
    <DialogHeader>
      <DialogTitle class="font-mono text-sm truncate" title={dlg.entry?.path}
        >{dlg.entry?.name}</DialogTitle
      >
      <DialogDescription class="font-mono text-xs break-all">{dlg.entry?.path}</DialogDescription>
    </DialogHeader>
    <div class="space-y-2">
      {#if dlg.loading}
        <div class="flex items-center justify-center gap-2 py-8 text-muted-foreground">
          <Spinner class="w-4 h-4" />
          <span class="text-sm">{t('common.loading')}</span>
        </div>
      {:else if dlg.error}
        <Alert variant="error">{dlg.error}</Alert>
      {:else}
        {#if dlg.truncated}
          <Alert variant="info" class="mb-2">{t('machines.sftpPreviewTruncated')}</Alert>
        {/if}
        <pre
          class="max-h-96 overflow-auto whitespace-pre-wrap break-all rounded-md border border-border bg-muted/50 p-3 font-mono text-xs">{dlg.text}</pre>
      {/if}
    </div>
    <DialogFooter>
      <Button variant="outline" onclick={() => (dlg.preview = false)} disabled={dlg.loading}
        >{t('common.close')}</Button
      >
    </DialogFooter>
  </DialogContent>
</DialogRoot>
