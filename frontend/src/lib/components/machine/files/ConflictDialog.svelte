<script>
  /**
   * ConflictDialog — every colliding name of one copy batch in a single
   * question. Answering "keep both" once is the difference between a
   * folder copy that needs one click and one that needs forty.
   */
  import { t } from '$lib/i18n.svelte.js';
  import { Button } from '$lib/components/ui/button';
  import {
    Dialog as DialogRoot,
    DialogContent,
    DialogHeader,
    DialogTitle,
    DialogDescription,
    DialogFooter,
  } from '$lib/components/ui/dialog';
  import { AlertTriangle } from '@lucide/svelte';

  let { open = $bindable(false), conflicts = [], busy = false, onresolve = () => {} } = $props();

  function choose(policy) {
    if (busy) return;
    onresolve(policy);
  }
</script>

<DialogRoot bind:open>
  <DialogContent class="sm:max-w-md [&>*]:min-w-0">
    <DialogHeader>
      <DialogTitle class="flex items-center gap-2">
        <AlertTriangle class="w-4 h-4 text-warning" />
        {t('files.conflictTitle', { n: conflicts.length })}
      </DialogTitle>
      <DialogDescription>{t('files.conflictDesc')}</DialogDescription>
    </DialogHeader>

    <ul class="max-h-48 overflow-auto rounded-md border border-border bg-muted/40 p-2 space-y-1">
      {#each conflicts as name (name)}
        <li class="font-mono text-xs truncate" title={name}>{name}</li>
      {/each}
    </ul>

    <DialogFooter class="gap-2 sm:justify-between">
      <Button variant="outline" size="sm" disabled={busy} onclick={() => choose('skip')}>
        {t('files.skipAll')}
      </Button>
      <Button variant="outline" size="sm" disabled={busy} onclick={() => choose('overwrite')}>
        {t('files.overwriteAll')}
      </Button>
      <Button size="sm" disabled={busy} onclick={() => choose('rename')}>
        {t('files.keepBoth')}
      </Button>
    </DialogFooter>
  </DialogContent>
</DialogRoot>
