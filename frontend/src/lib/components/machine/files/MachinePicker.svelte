<script>
  /**
   * MachinePicker — one pane's machine selector.
   *
   * Only ssh machines are offered (the file browser has nothing to show
   * for telnet/serial), and the list comes from the shared store so the
   * two panes and the sidebar never fetch it three times.
   */
  import { t } from '$lib/i18n.svelte.js';

  let {
    value = '',
    machines = [],
    disabled = false,
    busy = false,
    label = '',
    onpick = () => {},
  } = $props();

  function change(e) {
    onpick(e.target.value);
  }
</script>

<div class="flex items-center gap-2 min-w-0">
  <select
    class="input h-8 w-full max-w-[240px] min-w-0 truncate text-xs font-medium disabled:opacity-50"
    aria-label={label || t('files.pickMachine')}
    title={label}
    {disabled}
    {value}
    onchange={change}
  >
    <option value="">{t('files.pickMachine')}</option>
    {#each machines as m (m.id)}
      <option value={m.id}>{m.name}</option>
    {/each}
  </select>
  {#if busy}
    <span class="text-[11px] text-muted-foreground shrink-0">{t('common.loading')}</span>
  {/if}
</div>
