<script>
  /**
   * TransferQueue — one row per job, whatever produced it.
   *
   * A tree copy is a single aggregated row (files_done/files_total)
   * because a 2000-file folder that rendered 2000 rows would be a queue
   * nobody can read, and cancelling it needs batch semantics anyway.
   */
  import { t } from '$lib/i18n.svelte.js';
  import {
    ArrowDown,
    ArrowRightLeft,
    ArrowUp,
    Download,
    Eraser,
    Pause,
    Play,
    Trash2,
    X,
  } from '@lucide/svelte';
  import { Button } from '$lib/components/ui/button';
  import ProgressBar from '$lib/components/ProgressBar.svelte';
  import Spinner from '$lib/components/Spinner.svelte';
  import { formatBytes, formatETA, formatRate } from '$lib/utils/format.js';
  import { jobFileCount, jobPercent, isTerminalStatus } from '$lib/utils/fileTransfer.js';
  import { files } from '$lib/stores/files.svelte.js';
  import { saveJobResult } from './fileActions.js';
  import { toast } from '$lib/components/ui/toast';
  import { messageOf } from './paneModel.svelte.js';

  let {
    jobs = [],
    loading = false,
    emptyLabel = t('files.queueEmpty'),
    showMachine = false,
  } = $props();

  let clearing = $state(false);
  const finishedCount = $derived(jobs.filter((j) => isTerminalStatus(j.status)).length);

  function statusKey(status) {
    return (
      {
        running: 'machines.transferRunning',
        done: 'machines.transferDone',
        error: 'machines.transferError',
        canceled: 'machines.transferCanceled',
        paused: 'machines.transferPaused',
      }[status] || status
    );
  }

  function statusClass(status) {
    return (
      {
        done: 'bg-success/10 border-success/30 text-success',
        error: 'bg-destructive/10 border-destructive/30 text-destructive',
        canceled: 'bg-muted border-border text-muted-foreground',
        paused: 'bg-warning/10 border-warning/30 text-warning',
      }[status] || 'bg-accent/10 border-accent/30 text-accent'
    );
  }

  function title(job) {
    if (job.kind === 'copy') {
      const from = showMachine
        ? `${job.machine || ''} → ${job.dst_machine || ''}`
        : job.dst_machine || '';
      return `${from} · ${job.remote_path}`;
    }
    return job.remote_path || job.file_name || job.id;
  }

  async function cancel(job) {
    await files.cancel(job);
  }

  async function remove(job) {
    try {
      await files.remove(job);
    } catch (e) {
      toast.error(messageOf(e));
    }
  }

  async function clearFinished() {
    if (clearing || !finishedCount) return;
    clearing = true;
    try {
      await files.clearFinished();
    } catch (e) {
      toast.error(messageOf(e));
    } finally {
      clearing = false;
    }
  }

  async function pause(job) {
    await files.pauseJob(job);
  }

  async function resume(job) {
    await files.resumeJob(job);
  }
</script>

<div class="space-y-2">
  <div class="flex items-center justify-between">
    <span class="text-[11px] font-medium uppercase tracking-wide text-muted-foreground"
      >{t('files.queue')}</span
    >
    <div class="flex items-center gap-2">
      {#if finishedCount > 0}
        <Button
          type="button"
          variant="ghost"
          size="xs"
          disabled={clearing}
          title={t('files.queueClearFinished', { n: finishedCount })}
          aria-label={t('files.queueClearFinished', { n: finishedCount })}
          onclick={clearFinished}
        >
          <Eraser class="w-3 h-3" />
          {t('files.queueClearFinished', { n: finishedCount })}
        </Button>
      {/if}
      {#if loading}
        <Spinner class="w-3.5 h-3.5" />
      {/if}
    </div>
  </div>

  {#if jobs.length === 0 && !loading}
    <div class="py-2 text-sm text-muted-foreground">{emptyLabel}</div>
  {:else}
    <div class="space-y-2">
      {#each jobs as job (job.id)}
        {@const pct = jobPercent(job)}
        {@const count = jobFileCount(job)}
        <div class="rounded-lg border border-border px-3 py-2 space-y-1.5">
          <div class="flex items-center gap-2 min-w-0">
            {#if job.kind === 'upload'}
              <ArrowUp class="w-4 h-4 shrink-0 text-accent" />
            {:else if job.kind === 'copy'}
              <ArrowRightLeft class="w-4 h-4 shrink-0 text-accent" />
            {:else}
              <ArrowDown class="w-4 h-4 shrink-0 text-accent" />
            {/if}
            <span class="min-w-0 flex-1 truncate font-mono text-xs" title={title(job)}
              >{title(job)}</span
            >
            <span
              class="shrink-0 rounded border px-1.5 py-0.5 text-[10px] {statusClass(job.status)}"
            >
              {t(statusKey(job.status))}
            </span>
            <div class="flex shrink-0 items-center gap-0.5">
              {#if job.status === 'running'}
                <Button
                  type="button"
                  variant="ghost"
                  size="icon-xs"
                  title={t('files.queuePause')}
                  aria-label={t('files.queuePause')}
                  onclick={() => pause(job)}
                >
                  <Pause class="w-3.5 h-3.5" />
                </Button>
                <Button
                  type="button"
                  variant="ghost"
                  size="icon-xs"
                  title={t('files.queueCancel')}
                  aria-label={t('files.queueCancel')}
                  onclick={() => cancel(job)}
                >
                  <X class="w-3.5 h-3.5" />
                </Button>
              {:else if job.status === 'paused'}
                <Button
                  type="button"
                  variant="ghost"
                  size="icon-xs"
                  title={t('files.queueResume')}
                  aria-label={t('files.queueResume')}
                  onclick={() => resume(job)}
                >
                  <Play class="w-3.5 h-3.5" />
                </Button>
                <Button
                  type="button"
                  variant="ghost"
                  size="icon-xs"
                  title={t('files.queueCancel')}
                  aria-label={t('files.queueCancel')}
                  onclick={() => cancel(job)}
                >
                  <X class="w-3.5 h-3.5" />
                </Button>
              {:else if job.status === 'done' && job.kind === 'download'}
                <Button
                  type="button"
                  variant="ghost"
                  size="icon-xs"
                  title={t('machines.transferSave')}
                  aria-label={t('machines.transferSave')}
                  onclick={() => saveJobResult(job)}
                >
                  <Download class="w-3.5 h-3.5" />
                </Button>
              {/if}
              {#if job.status !== 'running' && job.status !== 'paused'}
                <Button
                  type="button"
                  variant="ghost"
                  size="icon-xs"
                  title={t('files.queueRemove')}
                  aria-label={t('files.queueRemove')}
                  onclick={() => remove(job)}
                >
                  <Trash2 class="w-3.5 h-3.5" />
                </Button>
              {/if}
            </div>
          </div>

          {#if pct != null}
            <ProgressBar
              value={pct}
              size="sm"
              variant={job.status === 'error' ? 'destructive' : 'accent'}
            />
          {/if}

          <div
            class="flex flex-wrap items-center gap-x-3 gap-y-1 text-[11px] text-muted-foreground tnum"
          >
            <span>{formatBytes(job.done || 0)} / {formatBytes(job.total || 0)}</span>
            {#if count.total > 1}
              <span>{t('files.queueFiles', { done: count.done, total: count.total })}</span>
            {/if}
            {#if job.skipped > 0}
              <span title={t('files.queueSkippedHint')}
                >{t('files.queueSkipped', { n: job.skipped })}</span
              >
            {/if}
            {#if job.status === 'running' && job.speed_bps}
              <span>{formatRate(job.speed_bps)}</span>
              {#if job.eta_seconds > 0}
                <span>{formatETA(job.eta_seconds)}</span>
              {/if}
            {/if}
          </div>

          {#if job.current_path}
            <div
              class="truncate font-mono text-[10px] text-muted-foreground/80"
              title={job.current_path}
            >
              {job.current_path}
            </div>
          {/if}
          {#if job.error}
            <div class="break-words text-xs text-destructive">{job.error}</div>
          {/if}
        </div>
      {/each}
    </div>
  {/if}
</div>
