<script>
  /**
   * FilesWorkspace — one machine's directory, with a transfer view that
   * only appears when it is asked for.
   *
   * The workspace lives in the machine detail tab, so the pane that is
   * always there is the machine whose page we are on. A second pane is
   * the tool for one job — moving files between two machines — and it
   * earns its width when that job is asked for, not before.
   */
  import { onMount, onDestroy } from 'svelte';
  import { t } from '$lib/i18n.svelte.js';
  import { toast } from '$lib/components/ui/toast';
  import { files } from '$lib/stores/files.svelte.js';
  import { dirOf, entriesFromPayload, isTerminalStatus } from '$lib/utils/fileTransfer.js';
  import { copyEntries, destDirOf } from './fileActions.js';
  import { messageOf, Pane } from './paneModel.svelte.js';
  import FilePane from './FilePane.svelte';
  import TransferQueue from './TransferQueue.svelte';

  let { machineId = '' } = $props();

  const left = new Pane({ side: 'left' });
  const right = new Pane({ side: 'right' });

  let transfer = $state(false);
  let focus = $state('left');
  let root = $state(null);
  const machines = $derived(files.fileMachines);
  const others = $derived(machines.filter((m) => m.id !== machineId));
  const jobs = $derived(machineId ? files.byMachine(machineId) : files.allJobs);

  /**
   * A finished job lands in a directory some pane may be showing, and
   * that listing is one step stale until someone asks again — the pane
   * that owns the destination asks.
   *
   * `seen` is a plain Set on purpose (it must not re-run this effect),
   * and only terminal jobs ever enter it: a job is invisible to the
   * effect while it is live, the frame that turns it done re-runs the
   * effect, the id is new, and its destination gets refreshed.
   *
   * Both panes are checked, not just the page's machine: in the split
   * view the copy destination is usually the OTHER pane (the right one),
   * and the row only shows here because byMachine matches either end
   * of the copy. Downloads never land on a machine, so they skip the
   * match entirely.
   */
  const seen = new Set();
  $effect(() => {
    const hit = new Set();
    for (const job of files.allJobs) {
      if (!isTerminalStatus(job.status) || seen.has(job.id)) continue;
      seen.add(job.id);
      let destId = '';
      let dirs = [];
      if (job.kind === 'copy') {
        destId = job.dst_machine_id;
        dirs = job.dst_dir ? [job.dst_dir] : [];
      } else if (job.kind === 'upload') {
        destId = job.machine_id;
        dirs = (job.remote_paths || [job.remote_path]).filter(Boolean).map(dirOf);
      } else continue;
      if (!destId || !dirs.length) continue;
      for (const pane of [left, right]) {
        if (pane.machineId !== destId) continue;
        // A new entry changes the listing of the destination itself and
        // of every ancestor above it, and of no subfolder below it.
        const matches = dirs.some(
          (d) =>
            pane.path === d || pane.path === '/' || (d !== '/' && d.startsWith(pane.path + '/'))
        );
        if (matches) hit.add(pane);
      }
    }
    // One refresh per pane per run: the pane's loader is version-guarded
    // but not in-flight-deduplicated, so N matching jobs would fire N
    // identical requests without this.
    for (const pane of hit) pane.refresh();
  });

  async function restore() {
    files.ensureSSE();
    await files.loadMachines();
    if (!machineId) return;
    left.setMachine(machineId);
    await left.load('');
    files.refreshQueue(machines.map((m) => m.id));
  }

  /**
   * Open (or close) the second pane. Opening picks a destination so the
   * copy is one selection away; a pane the user already aimed at is left
   * where it was, and closing only hides it — the model survives, so the
   * listing is still there when the transfer view comes back.
   */
  function openTransfer() {
    if (!others.length) return;
    transfer = true;
    focus = 'right';
    if (right.machineId) return;
    right.setMachine(others[0].id);
    right.load('');
  }

  function closeTransfer() {
    transfer = false;
    focus = 'left';
  }

  // Window-level because the two panes are separate tab sequences: a
  // pane that has not been clicked yet still has to answer F6. The
  // containment test keeps the shortcut from stealing it from a terminal
  // or dialog mounted somewhere else on the page.
  function keydown(e) {
    if (e.key !== 'F6' || !transfer || !root?.contains(e.target)) return;
    e.preventDefault();
    focus = focus === 'left' ? 'right' : 'left';
  }

  /**
   * A drag from one pane to the other. There is no dialog for a pointer
   * gesture, so collisions keep both names — of the three policies the
   * server understands, that is the only one that cannot overwrite.
   */
  async function onDropInternal(payload, target) {
    if (!transfer || !payload?.m || payload.m === target.machineId) return;
    const entries = entriesFromPayload(payload);
    if (!entries.length) return;
    try {
      const res = await copyEntries(
        payload.m,
        entries,
        target.machineId,
        destDirOf(target),
        'rename'
      );
      if (res.errors.length) toast.error(res.errors.join('\n'));
    } catch (e) {
      toast.error(messageOf(e));
    }
  }

  onMount(restore);
  onDestroy(() => {
    left.dispose();
    right.dispose();
  });
</script>

<svelte:window onkeydown={keydown} />

<div bind:this={root} class="flex min-h-0 w-full flex-1 flex-col gap-2">
  <div class="flex min-h-0 w-full flex-1 gap-2" role="group" aria-label={t('files.title')}>
    <section class="flex min-h-0 min-w-0 flex-col" style="flex:{transfer ? files.split : 100} 1 0%">
      <FilePane
        pane={left}
        other={transfer ? right : null}
        side="left"
        {machines}
        focused={focus === 'left'}
        pinned
        openTransfer={transfer ? null : openTransfer}
        closeTransfer={transfer ? closeTransfer : null}
        hasOther={others.length > 0}
        onfocus={() => (focus = 'left')}
        ondropinternal={onDropInternal}
      />
    </section>

    {#if transfer}
      <div
        class="w-1.5 shrink-0 cursor-col-resize self-stretch rounded-full bg-border/60 transition-colors hover:bg-accent/50"
        role="separator"
        aria-orientation="vertical"
        title={t('files.splitHint')}
        onpointerdown={(e) => {
          e.preventDefault();
          const move = (ev) => {
            const box = root?.getBoundingClientRect();
            if (!box || box.width <= 0) return;
            files.setSplit(((ev.clientX - box.left) / box.width) * 100);
          };
          const stop = () => {
            window.removeEventListener('pointermove', move);
            window.removeEventListener('pointerup', stop);
          };
          window.addEventListener('pointermove', move);
          window.addEventListener('pointerup', stop);
        }}
      ></div>

      <section class="flex min-h-0 min-w-0 flex-col" style="flex:{100 - files.split} 1 0%">
        <FilePane
          pane={right}
          other={left}
          side="right"
          {machines}
          focused={focus === 'right'}
          {closeTransfer}
          onfocus={() => (focus = 'right')}
          ondropinternal={onDropInternal}
        />
      </section>
    {/if}
  </div>

  {#if jobs.length}
    <div
      class="max-h-[45%] shrink-0 overflow-auto rounded-lg border border-border bg-card/40 p-3"
      aria-label={t('files.queue')}
    >
      <TransferQueue {jobs} loading={files.queueLoading} showMachine={true} />
    </div>
  {/if}
</div>
