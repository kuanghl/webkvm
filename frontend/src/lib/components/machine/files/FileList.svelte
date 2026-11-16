<script>
  /**
   * FileList — the rows of one pane.
   *
   * Presentational plus keyboard: it owns focus and selection mechanics
   * and hands back everything that needs a dialog or a toast. Rows are
   * focusable because the single-pane browser used <tr onclick>, which
   * locked out both the keyboard and any cursor that is not a mouse.
   */
  import { t } from '$lib/i18n.svelte.js';
  import { FileText, Folder } from '@lucide/svelte';
  import { formatBytes } from '$lib/utils/format.js';
  import { DRAG_TYPE, dropPayload, flattenDrop } from '$lib/utils/fileTransfer.js';

  let {
    pane,
    focused = false,
    onopen = () => {},
    onpreview = () => {},
    ondelete = () => {},
    ondropitems = () => {},
    ondropinternal = () => {},
  } = $props();

  let dropArmed = $state(false);
  let selectAllBox = $state(null);
  // Select-all matches the keyboard (Ctrl+A): every sorted row, not just
  // the paged slice.
  const allSelected = $derived(
    pane.sorted.length > 0 && pane.sorted.every((e) => pane.selected.has(e.path))
  );
  const someSelected = $derived(!allSelected && pane.sorted.some((e) => pane.selected.has(e.path)));
  // `indeterminate` is a DOM-only property (no attribute form exists),
  // so the effect pushes it onto the element straight; the local read
  // keeps the write target the element, not the state variable.
  $effect(() => {
    const box = selectAllBox;
    if (box) box.indeterminate = someSelected && !allSelected;
  });

  function fmtTime(sec) {
    if (!sec) return '—';
    const d = new Date(sec * 1000);
    return Number.isNaN(d.getTime()) ? '—' : d.toLocaleString();
  }

  function rowClick(e, entry, i) {
    if (e.button !== 0) return;
    pane.active = i;
    if (e.ctrlKey || e.metaKey || e.shiftKey) pane.toggle(entry);
    else pane.selectOnly(entry);
  }

  function keydown(e) {
    const entry = pane.sorted[pane.active];
    // Alt+arrows walk the pane's history and belong to the pane, which
    // also owns the back/forward buttons.
    if (e.altKey) return;
    if (e.ctrlKey || e.metaKey) {
      if (e.key.toLowerCase() === 'a') {
        e.preventDefault();
        pane.selectAll();
      }
      return;
    }
    switch (e.key) {
      case 'ArrowDown':
        e.preventDefault();
        pane.moveActive(1);
        break;
      case 'ArrowUp':
        e.preventDefault();
        pane.moveActive(-1);
        break;
      case 'Enter':
        e.preventDefault();
        if (entry?.is_dir) pane.open(entry);
        else if (entry) onpreview(entry);
        break;
      case ' ':
        e.preventDefault();
        if (entry) pane.toggle(entry);
        break;
      case 'Backspace':
        e.preventDefault();
        pane.up();
        break;
      case 'Delete':
        e.preventDefault();
        ondelete();
        break;
      case 'Escape':
        pane.clearSelection();
        break;
      default:
        // Type-ahead: bare printable characters jump to the first name
        // that starts with the buffer, which 600 ms of keystrokes holds.
        if (e.key.length === 1) {
          e.preventDefault();
          pane.typeAhead(e.key);
        }
    }
  }

  function onDragStart(e, entry) {
    const picked = pane.selected.has(entry.path)
      ? pane.sorted.filter((x) => pane.selected.has(x.path))
      : [entry];
    if (!pane.selected.has(entry.path)) pane.selectOnly(entry);
    e.dataTransfer.effectAllowed = 'copy';
    e.dataTransfer.setData(DRAG_TYPE, dropPayload(pane.machineId, pane.path, picked));
  }

  function accepts(dataTransfer) {
    const types = Array.from(dataTransfer?.types || []);
    return types.includes(DRAG_TYPE) || types.includes('Files');
  }

  function onDragOver(e) {
    if (!accepts(e.dataTransfer)) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = 'copy';
    dropArmed = true;
  }

  async function onDrop(e) {
    e.preventDefault();
    dropArmed = false;
    const payload = e.dataTransfer?.getData(DRAG_TYPE);
    if (payload) {
      try {
        ondropinternal(JSON.parse(payload));
      } catch {
        /* a payload we cannot read is not a drop we can honour */
      }
      return;
    }
    ondropitems(await flattenDrop(e.dataTransfer));
  }
</script>

<!-- The rows are the tab stops; this wrapper only has to be focusable
     itself so a click below the last row still routes the arrow keys to
     the pane. -->
<!-- svelte-ignore a11y_no_noninteractive_tabindex -->
<!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
<div
  class="relative flex-1 min-h-0 overflow-auto rounded-md border {dropArmed
    ? 'border-accent/70 bg-accent/5'
    : 'border-border'}"
  role="region"
  aria-label={t('files.listLabel')}
  tabindex="0"
  onkeydown={keydown}
  ondragover={onDragOver}
  ondragleave={() => (dropArmed = false)}
  ondrop={onDrop}
>
  {#if pane.visible.length}
    <table class="w-full text-sm">
      <thead
        class="sticky top-0 z-10 bg-card/95 backdrop-blur text-[11px] uppercase tracking-wide text-muted-foreground"
      >
        <tr class="[&>th]:px-3 [&>th]:py-1.5 [&>th]:text-left [&>th]:font-medium">
          <th class="w-8">
            <input
              bind:this={selectAllBox}
              type="checkbox"
              class="h-3.5 w-3.5 cursor-pointer accent-accent"
              aria-label={t('files.selectAll')}
              checked={allSelected}
              onchange={() => (allSelected ? pane.clearSelection() : pane.selectAll())}
            />
          </th>
          <th>{t('files.name')}</th>
          <th class="w-24 text-right">{t('files.size')}</th>
          <th class="w-24">{t('files.permissions')}</th>
          <th class="w-40">{t('files.modified')}</th>
        </tr>
      </thead>
      <tbody>
        {#each pane.visible as entry, i (entry.path)}
          <tr
            tabindex={i === pane.active ? 0 : -1}
            draggable="true"
            aria-selected={pane.selected.has(entry.path)}
            class="cursor-default outline-none border-b border-border/50 hover:bg-muted/40 focus:bg-muted/60
              [&>td]:px-3 [&>td]:py-1.5 {pane.selected.has(entry.path) ? 'bg-accent/10' : ''}
              {i === pane.active && focused ? 'ring-1 ring-inset ring-accent/50' : ''}"
            onmousedown={(e) => rowClick(e, entry, i)}
            ondblclick={() => onopen(entry)}
            onfocus={() => (pane.active = i)}
            ondragstart={(e) => onDragStart(e, entry)}
          >
            <td
              class="w-8"
              onmousedown={(e) => e.stopPropagation()}
              onkeydown={(e) => e.stopPropagation()}
            >
              <input
                type="checkbox"
                class="h-3.5 w-3.5 cursor-pointer accent-accent"
                checked={pane.selected.has(entry.path)}
                aria-label={entry.name}
                onchange={() => pane.toggle(entry)}
              />
            </td>
            <td class="min-w-0">
              <div class="flex items-center gap-2 min-w-0">
                {#if entry.is_dir}
                  <Folder class="w-4 h-4 shrink-0 text-accent" />
                {:else}
                  <FileText class="w-4 h-4 shrink-0 text-muted-foreground" />
                {/if}
                <span class="truncate font-mono text-xs" title={entry.path}>{entry.name}</span>
              </div>
            </td>
            <td class="w-24 text-right tnum text-xs text-muted-foreground">
              {#if entry.is_dir}
                {@const sz = pane.dirSizes.get(entry.path)}
                {#if !sz}
                  <span aria-label={t('files.dirSizePending')}>…</span>
                {:else if sz.error}
                  <span
                    title={t('files.dirSizeUnavailable')}
                    aria-label={t('files.dirSizeUnavailable')}>—</span
                  >
                {:else}
                  <span title={sz.truncated ? t('files.dirSizeApprox') : undefined}>
                    {sz.truncated ? '≈ ' : ''}{formatBytes(sz.size || 0)}
                  </span>
                {/if}
              {:else}
                {formatBytes(entry.size || 0)}
              {/if}
            </td>
            <td class="w-24 font-mono text-[11px] text-muted-foreground">{entry.mode || ''}</td>
            <td class="w-40 text-[11px] tnum text-muted-foreground">{fmtTime(entry.mod_time)}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  {:else if !pane.loading}
    <div class="p-6 text-center text-sm text-muted-foreground">{t('files.emptyDir')}</div>
  {/if}

  {#if pane.hasMore}
    <button
      type="button"
      class="w-full py-2 text-xs text-accent transition-colors hover:bg-muted/40"
      onclick={() => pane.loadMore()}
    >
      {t('files.showingOf', { shown: pane.visible.length, total: pane.totalVisible })} · {t(
        'files.loadMore'
      )}
    </button>
  {/if}

  {#if pane.loading && !pane.entries.length}
    <div class="p-6 text-center text-sm text-muted-foreground">{t('common.loading')}</div>
  {/if}
</div>
