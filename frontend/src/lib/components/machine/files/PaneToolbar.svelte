<script>
  /**
   * PaneToolbar — the machine selector and the row operations of one
   * pane. Buttons state is derived from the selection, so FilePane only
   * passes the selected count and the single entry.
   */
  import { t } from '$lib/i18n.svelte.js';
  import {
    ArrowDownToLine,
    ArrowRightLeft,
    Eye,
    EyeOff,
    FolderUp,
    Lock,
    Pencil,
    Plus,
    RefreshCw,
    Trash2,
    Upload,
    X,
  } from '@lucide/svelte';
  import { Button } from '$lib/components/ui/button';
  import MachinePicker from './MachinePicker.svelte';

  let {
    pane,
    actions,
    machines = [],
    other = null,
    machineName = '',
    otherName = '',
    count = 0,
    single = null,
    pinned = false,
    compact = false,
    canCopy = true,
    hasOther = false,
    busy = false,
    // Props, not `actions` members: a plain object would freeze which of
    // the two buttons this pane shows.
    openTransfer = null,
    closeTransfer = null,
  } = $props();

  const many = () => count > 0;
  const one = () => count === 1 && !!single;
</script>

<div class="flex flex-wrap items-center gap-1.5">
  <MachinePicker
    {machines}
    value={pane.machineId}
    disabled={pinned}
    busy={pane.loading}
    label={machineName}
    onpick={(id) => actions.pick(id)}
  />
  {#if !compact}
    <Button
      type="button"
      variant="ghost"
      size="icon-xs"
      title={t('files.refresh')}
      aria-label={t('files.refresh')}
      disabled={!pane.machineId || busy}
      onclick={() => actions.refresh()}
    >
      <RefreshCw class="w-3.5 h-3.5" />
    </Button>
    <Button
      type="button"
      variant="ghost"
      size="icon-xs"
      title={t('files.newFolder')}
      aria-label={t('files.newFolder')}
      disabled={!pane.path || busy}
      onclick={() => actions.mkdir()}
    >
      <Plus class="w-3.5 h-3.5" />
    </Button>
    <Button
      type="button"
      variant="ghost"
      size="icon-xs"
      title={t('files.upload')}
      aria-label={t('files.upload')}
      disabled={!pane.path || busy}
      onclick={actions.chooseFiles}
    >
      <Upload class="w-3.5 h-3.5" />
    </Button>
    <Button
      type="button"
      variant="ghost"
      size="icon-xs"
      title={t('files.uploadFolder')}
      aria-label={t('files.uploadFolder')}
      disabled={!pane.path || busy}
      onclick={actions.chooseFolder}
    >
      <FolderUp class="w-3.5 h-3.5" />
    </Button>
    <Button
      type="button"
      variant="ghost"
      size="icon-xs"
      title={t('files.download')}
      aria-label={t('files.download')}
      disabled={!many() || busy}
      onclick={actions.download}
    >
      <ArrowDownToLine class="w-3.5 h-3.5" />
    </Button>
    <Button
      type="button"
      variant="ghost"
      size="icon-xs"
      title={t('files.rename')}
      aria-label={t('files.rename')}
      disabled={!one() || busy}
      onclick={() => actions.rename(single)}
    >
      <Pencil class="w-3.5 h-3.5" />
    </Button>
    <Button
      type="button"
      variant="ghost"
      size="icon-xs"
      title={t('files.chmod')}
      aria-label={t('files.chmod')}
      disabled={!one() || busy}
      onclick={() => actions.chmod(single)}
    >
      <Lock class="w-3.5 h-3.5" />
    </Button>
    <Button
      type="button"
      variant="ghost"
      size="icon-xs"
      title={t('files.preview')}
      aria-label={t('files.preview')}
      disabled={!one() || single?.is_dir || busy}
      onclick={() => actions.preview(single)}
    >
      <Eye class="w-3.5 h-3.5" />
    </Button>
    <Button
      type="button"
      variant="ghost"
      size="icon-xs"
      title={t('files.delete')}
      aria-label={t('files.delete')}
      disabled={!many() || busy}
      onclick={actions.askDelete}
    >
      <Trash2 class="w-3.5 h-3.5 text-destructive" />
    </Button>
    {#if openTransfer}
      <div class="mx-0.5 h-4 w-px bg-border" aria-hidden="true"></div>
      <Button
        type="button"
        variant="outline"
        size="sm"
        class="gap-1.5"
        disabled={!hasOther || busy}
        title={hasOther ? '' : t('files.transferNoTarget')}
        onclick={openTransfer}
      >
        <ArrowRightLeft class="w-3.5 h-3.5" />
        {t('files.transferOpen')}
      </Button>
    {:else if canCopy && other}
      <div class="mx-0.5 h-4 w-px bg-border" aria-hidden="true"></div>
      <Button
        type="button"
        variant="outline"
        size="sm"
        class="gap-1.5"
        disabled={!pane.machineId || !other.machineId || !many() || busy}
        onclick={actions.copy}
      >
        <ArrowRightLeft class="w-3.5 h-3.5" />
        {t('files.copyToMachine', { name: otherName })}
      </Button>
      {#if closeTransfer}
        <Button
          type="button"
          variant="ghost"
          size="icon-xs"
          title={t('files.transferClose')}
          aria-label={t('files.transferClose')}
          onclick={closeTransfer}
        >
          <X class="w-3.5 h-3.5" />
        </Button>
      {/if}
    {/if}
  {/if}
  <div class="ml-auto flex items-center gap-0.5">
    <Button
      type="button"
      variant={pane.sort === 'name' ? 'secondary' : 'ghost'}
      size="sm"
      onclick={() => pane.setSort('name')}>{t('files.sortName')}</Button
    >
    <Button
      type="button"
      variant={pane.sort === 'size' ? 'secondary' : 'ghost'}
      size="sm"
      onclick={() => pane.setSort('size')}>{t('files.sortSize')}</Button
    >
    <Button
      type="button"
      variant={pane.sort === 'time' ? 'secondary' : 'ghost'}
      size="sm"
      onclick={() => pane.setSort('time')}>{t('files.sortTime')}</Button
    >
    <Button
      type="button"
      variant="ghost"
      size="icon-xs"
      class="ml-1"
      aria-pressed={pane.showHidden}
      title={t('files.toggleHidden')}
      onclick={() => pane.toggleHidden()}
    >
      {#if pane.showHidden}
        <EyeOff class="w-3.5 h-3.5" />
      {:else}
        <Eye class="w-3.5 h-3.5" />
      {/if}
    </Button>
  </div>
</div>
