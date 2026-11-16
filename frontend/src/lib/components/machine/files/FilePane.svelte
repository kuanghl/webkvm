<script>
  /**
   * FilePane — one column of the workspace: machine, operations, path,
   * rows and its own host-key trust flow.
   *
   * A 409 from any SFTP call is a host-key problem rather than a
   * filesystem one, so every action runs through guard(): the pin dialog
   * takes over and no toast tells the user "hostkey_untrusted" as if it
   * were something they did wrong.
   *
   * Keyboard that spans panes (F6, Alt+arrows, Ctrl+Shift+arrows) is the
   * workspace's: this component only owns what a single machine can do.
   */
  import { t } from '$lib/i18n.svelte.js';
  import Alert from '$lib/components/Alert.svelte';
  import ConfirmDialog from '$lib/components/ConfirmDialog.svelte';
  import HostKeyDialog from '$lib/components/machine/HostKeyDialog.svelte';
  import ProgressBar from '$lib/components/ProgressBar.svelte';
  import { toast } from '$lib/components/ui/toast';
  import { files } from '$lib/stores/files.svelte.js';
  import { basename } from '$lib/utils/fileTransfer.js';
  import {
    conflictsFor,
    copyEntries,
    deleteEntries,
    destDirOf,
    downloadEntries,
    previewEntry,
    uploadToPane,
  } from './fileActions.js';
  import { hostKeyOf, messageOf } from './paneModel.svelte.js';
  import PaneToolbar from './PaneToolbar.svelte';
  import PathBreadcrumb from './PathBreadcrumb.svelte';
  import FileList from './FileList.svelte';
  import EntryDialogs from './EntryDialogs.svelte';
  import ConflictDialog from './ConflictDialog.svelte';

  let {
    pane,
    other = null,
    side = 'left',
    machines = [],
    focused = false,
    pinned = false,
    compact = false,
    canCopy = true,
    hasOther = false,
    openTransfer = null,
    closeTransfer = null,
    onfocus = () => {},
    ondropinternal = () => {},
  } = $props();

  let dlg = $state({
    mkdir: false,
    rename: false,
    chmod: false,
    preview: false,
    entry: null,
    value: '',
    text: '',
    truncated: false,
    loading: false,
    busy: false,
    error: '',
  });
  let del = $state({ open: false, entries: [] });
  let hkOpen = $state(false);
  let conflict = $state({ open: false, dst: null, names: [], entries: [] });
  let uploading = $state(null);
  let busyOp = $state(false);
  let fileInput = $state(null);
  let folderInput = $state(null);

  const machineName = $derived(files.machineName(pane.machineId));
  const otherName = $derived(other ? files.machineName(other.machineId) : '');
  const selection = $derived(pane.selectedEntries);

  // The pane owns the trust question because a listing can fail with it
  // too (see Pane.load); this only mirrors it into the dialog's open flag.
  $effect(() => {
    if (pane.hostKey) hkOpen = true;
  });
  $effect(() => {
    if (!hkOpen && pane.hostKey) pane.hostKey = null;
  });

  function raiseHostKey(e) {
    const hk = hostKeyOf(e);
    if (!hk) return false;
    pane.hostKey = hk;
    return true;
  }

  async function guard(fn) {
    if (busyOp) return;
    busyOp = true;
    try {
      await fn();
    } catch (e) {
      if (!raiseHostKey(e)) toast.error(messageOf(e));
    } finally {
      busyOp = false;
    }
  }

  function openDialog(kind, entry, value = '') {
    dlg.entry = entry || null;
    dlg.value = value;
    dlg.error = '';
    dlg.text = '';
    dlg.truncated = false;
    dlg[kind] = true;
  }

  function pick(id) {
    pane.setMachine(id);
    if (id) guard(() => pane.load(''));
  }

  function preview(entry) {
    if (!entry || entry.is_dir) return;
    openDialog('preview', entry);
    guard(async () => {
      dlg.loading = true;
      try {
        const res = await previewEntry(pane, entry);
        dlg.text = res.text;
        dlg.truncated = res.truncated;
      } catch (e) {
        if (!raiseHostKey(e)) dlg.error = messageOf(e);
      } finally {
        dlg.loading = false;
      }
    });
  }

  function download() {
    if (selection.length) guard(() => downloadEntries(pane, selection, t('files.batchFileName')));
  }

  function askDelete() {
    if (selection.length) del = { open: true, entries: selection };
  }

  function doDelete() {
    guard(async () => {
      const errors = await deleteEntries(pane, del.entries);
      del = { open: false, entries: [] };
      if (errors.length) toast.error(errors.join('\n'));
    });
  }

  function chooseFiles() {
    fileInput?.click();
  }

  function onPicked(e) {
    const items = Array.from(e.target.files || []).map((file) => ({
      file,
      relPath: file.webkitRelativePath || file.name,
    }));
    e.target.value = '';
    if (items.length) uploadItems(items);
  }

  function uploadItems(items) {
    guard(async () => {
      const res = await uploadToPane(pane, items, (p) => (uploading = p));
      if (res.errors.length) toast.error(res.errors.join('\n'));
      else if (res.uploaded) toast.success(t('files.uploadQueued', { n: res.uploaded }));
    });
  }

  /**
   * Copy toward the other pane. One batched stat first: conflicts are
   * answered as a single dialog over the whole selection, never as one
   * prompt per colliding name.
   */
  function copy() {
    const dst = other;
    if (!dst?.machineId || !pane.machineId) {
      toast.error(t('files.pickMachine'));
      return;
    }
    // Read once, before the first await: the answer to a question must
    // stay about the rows that were on screen when it was asked.
    const batch = selection;
    if (!batch.length) return;
    guard(async () => {
      const names = await conflictsFor(batch, dst);
      if (names.length) {
        // The selection is captured with the question: by the time the
        // user answers, the rows may have moved under a new listing.
        conflict = { open: true, dst, names, entries: batch };
        return;
      }
      await apply('overwrite', { dst, entries: batch });
    });
  }

  async function apply(policy, batch = conflict) {
    conflict = { open: false, dst: null, names: [], entries: [] };
    if (!batch?.dst || !batch.entries?.length) return;
    try {
      const res = await copyEntries(
        pane.machineId,
        batch.entries,
        batch.dst.machineId,
        destDirOf(batch.dst),
        policy
      );
      if (res.errors.length) toast.error(res.errors.join('\n'));
    } catch (e) {
      if (!raiseHostKey(e)) toast.error(messageOf(e));
    }
  }

  // Ctrl+Shift+arrow copies the focused pane's selection across; the
  // bare arrows stay free for the terminal-style back/forward a user
  // expects from them (Alt+arrow is the history walker here).
  function keydown(e) {
    if (!focused) return;
    if (e.altKey && (e.key === 'ArrowLeft' || e.key === 'ArrowRight')) {
      e.preventDefault();
      guard(() => (e.key === 'ArrowLeft' ? pane.back() : pane.forward()));
      return;
    }
    if (
      e.ctrlKey &&
      e.shiftKey &&
      other?.machineId &&
      (e.key === 'ArrowRight' || e.key === 'ArrowLeft')
    ) {
      const outward =
        (side === 'left' && e.key === 'ArrowRight') || (side === 'right' && e.key === 'ArrowLeft');
      if (outward) {
        e.preventDefault();
        copy();
      }
    }
  }

  const actions = {
    pick,
    copy,
    refresh: () => guard(() => pane.refresh()),
    mkdir: () => openDialog('mkdir'),
    rename: (entry) => openDialog('rename', entry, entry?.name || ''),
    chmod: (entry) => openDialog('chmod', entry),
    preview,
    download,
    askDelete,
    chooseFiles,
    chooseFolder: () => folderInput?.click(),
  };
</script>

<!-- A click anywhere in the pane (toolbar, breadcrumbs, empty space) is
     what makes it the pane the keyboard belongs to. -->
<!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
<div
  class="flex min-h-0 min-w-0 flex-1 flex-col gap-2 outline-none {focused
    ? 'rounded-lg ring-2 ring-accent/50'
    : ''}"
  role="group"
  aria-label={side === 'left' ? t('files.paneLeft') : t('files.paneRight')}
  onclick={() => onfocus()}
  onkeydown={keydown}
>
  <PaneToolbar
    {pane}
    {actions}
    {machines}
    {other}
    {machineName}
    {otherName}
    {pinned}
    {compact}
    {canCopy}
    {hasOther}
    {openTransfer}
    {closeTransfer}
    busy={busyOp}
    count={selection.length}
    single={selection.length === 1 ? selection[0] : null}
  />

  <PathBreadcrumb
    path={pane.path}
    home={pane.home}
    disabled={!pane.machineId}
    canBack={pane.canBack}
    canForward={pane.canForward}
    onback={() => guard(() => pane.back())}
    onforward={() => guard(() => pane.forward())}
    onnavigate={(target) =>
      guard(() =>
        target === 'up' ? pane.up() : target === 'home' ? pane.goHome() : pane.load(target)
      )}
  />

  {#if pane.error}
    <Alert variant="error" class="py-2 text-xs">{pane.error}</Alert>
  {/if}
  {#if uploading}
    <div class="flex items-center gap-2 text-[11px] text-muted-foreground">
      <span class="truncate font-mono">{basename(uploading.path)}</span>
      <ProgressBar value={uploading.pct} size="sm" class="w-32" />
    </div>
  {/if}

  {#if pane.machineId}
    <FileList
      {pane}
      {focused}
      onopen={(entry) => pane.open(entry)}
      onpreview={preview}
      ondelete={askDelete}
      ondropitems={uploadItems}
      ondropinternal={(payload) => ondropinternal(payload, pane)}
    />
  {:else}
    <div
      class="flex flex-1 items-center justify-center rounded-md border border-dashed border-border p-6 text-center text-sm text-muted-foreground"
    >
      {t('files.noMachine')}
    </div>
  {/if}

  <!-- sr-only, not display:none: a file input that is not rendered does
       not open its picker when the button clicks it programmatically. -->
  <input
    bind:this={fileInput}
    type="file"
    multiple
    tabindex="-1"
    class="sr-only"
    onchange={onPicked}
  />
  <!-- The folder picker: webkitdirectory keeps the tree shape in
       webkitRelativePath, so onPicked's relPath mapping covers it as is. -->
  <input
    bind:this={folderInput}
    type="file"
    multiple
    directory
    webkitdirectory
    tabindex="-1"
    class="sr-only"
    onchange={onPicked}
  />
</div>

{#if pane.machineId}
  <EntryDialogs {pane} {dlg} />
{/if}

<ConfirmDialog
  open={del.open}
  title={del.entries.length === 1
    ? t('files.confirmDelete', { name: del.entries[0].name })
    : t('files.confirmDeleteMany', { n: del.entries.length })}
  description={t('files.confirmDeleteDesc')}
  confirmLabel={t('files.delete')}
  variant="destructive"
  loading={busyOp}
  onConfirm={doDelete}
  onCancel={() => (del = { open: false, entries: [] })}
/>

<ConflictDialog
  bind:open={conflict.open}
  conflicts={conflict.names}
  busy={busyOp}
  onresolve={apply}
/>

{#if pane.machineId}
  <HostKeyDialog
    bind:open={hkOpen}
    machineId={pane.machineId}
    {machineName}
    presented={pane.hostKey?.presented || ''}
    pinned={pane.hostKey?.pinned || ''}
    onPinned={() => guard(() => pane.refresh())}
  />
{/if}
