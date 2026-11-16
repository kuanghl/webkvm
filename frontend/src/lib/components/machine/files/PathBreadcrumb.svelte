<script>
  /**
   * PathBreadcrumb — home, one crumb per segment, and the up-one button
   * the single-pane browser never had (its only way out of a directory
   * was a crumb click).
   *
   * The last crumb is a path bar: the browser could read the location
   * but not change it, which is where "just let me type /home/user"
   * was left unsaid.
   */
  import { t } from '$lib/i18n.svelte.js';
  import { ChevronLeft, ChevronRight, Home, ArrowUp } from '@lucide/svelte';
  import { Button } from '$lib/components/ui/button';
  import { joinPath, dirOf } from '$lib/utils/fileTransfer.js';

  let {
    path = '',
    home = '',
    disabled = false,
    onnavigate = () => {},
    onback = () => {},
    onforward = () => {},
    canBack = false,
    canForward = false,
  } = $props();

  // Absolute path → cumulative crumbs, so each label is a real target.
  const crumbs = $derived.by(() => {
    const out = [];
    if (path && path !== '/') {
      let acc = '';
      for (const part of path.split('/').filter(Boolean)) {
        acc += '/' + part;
        out.push({ label: part, path: acc });
      }
    }
    return out;
  });

  // The display follows the path prop while untouched; typing sets the
  // override, and both a blur that did not navigate and a successful
  // navigation (the prop lands on the typed value) clear it.
  let typed = $state('');
  let dirty = $state(false);
  const display = $derived(dirty ? typed : path);

  // A leading "/" is absolute; anything else is relative to the current
  // folder — the same rule the shell gives you, which is what "type a
  // path and jump" should mean.
  function resolveDraft(v) {
    const s = (v || '').trim();
    if (!s || s === path) return null;
    return s.startsWith('/') ? s : joinPath(dirOf(path), s);
  }

  function go() {
    const target = resolveDraft(display);
    if (target) onnavigate(target);
  }

  function revert() {
    dirty = false;
  }
</script>

<div
  class="flex items-center gap-1 min-w-0 text-xs"
  role="navigation"
  aria-label={t('files.breadcrumb')}
>
  <Button
    type="button"
    variant="ghost"
    size="icon-xs"
    class="shrink-0"
    title={t('files.historyBack')}
    aria-label={t('files.historyBack')}
    disabled={!canBack}
    onclick={onback}
  >
    <ChevronLeft class="w-3.5 h-3.5" />
  </Button>
  <Button
    type="button"
    variant="ghost"
    size="icon-xs"
    class="shrink-0"
    title={t('files.historyForward')}
    aria-label={t('files.historyForward')}
    disabled={!canForward}
    onclick={onforward}
  >
    <ChevronRight class="w-3.5 h-3.5" />
  </Button>
  <Button
    type="button"
    variant="ghost"
    size="icon-xs"
    class="shrink-0"
    title={t('files.up')}
    aria-label={t('files.up')}
    disabled={disabled || path === '/'}
    onclick={() => onnavigate('up')}
  >
    <ArrowUp class="w-3.5 h-3.5" />
  </Button>
  <Button
    type="button"
    variant="ghost"
    size="icon-xs"
    class="shrink-0"
    title={home || t('files.home')}
    aria-label={t('files.home')}
    disabled={disabled || !home}
    onclick={() => onnavigate('home')}
  >
    <Home class="w-3.5 h-3.5" />
  </Button>
  <div class="flex items-center gap-0.5 min-w-0 overflow-x-auto">
    {#if !path}
      <span class="px-1 font-mono text-muted-foreground shrink-0">/</span>
    {:else}
      {#each crumbs as crumb, i (crumb.path)}
        {#if i > 0}
          <ChevronRight class="w-3 h-3 text-muted-foreground/60 shrink-0" aria-hidden="true" />
        {/if}
        {#if i < crumbs.length - 1}
          <button
            type="button"
            class="px-1 py-0.5 rounded font-mono truncate max-w-[10rem] hover:bg-muted transition-colors text-muted-foreground"
            title={crumb.path}
            {disabled}
            onclick={() => onnavigate(crumb.path)}
          >
            {crumb.label}
          </button>
        {/if}
      {/each}
      {#if crumbs.length > 0}
        <ChevronRight class="w-3 h-3 text-muted-foreground/60 shrink-0" aria-hidden="true" />
      {/if}
    {/if}
    <!-- The last crumb is a path bar: type (optionally absolute) and
         Enter jumps; a blur that did not navigate reverts the display. -->
    <input
      value={display}
      type="text"
      spellcheck="false"
      autocomplete="off"
      placeholder={t('files.pathPlaceholder')}
      aria-label={t('files.breadcrumb')}
      class="w-48 shrink px-1 py-0.5 font-mono text-foreground font-medium rounded outline-none bg-transparent hover:bg-muted focus:bg-muted"
      {disabled}
      oninput={(e) => {
        typed = e.target.value;
        dirty = true;
      }}
      onkeydown={(e) => {
        if (e.key === 'Enter') {
          e.preventDefault();
          go();
          e.currentTarget.blur();
        }
      }}
      onblur={revert}
    />
  </div>
</div>
