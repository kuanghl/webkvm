<script>
  /**
   * MachinesList — SSH Machines catalog (VM Machines style).
   *
   * Cards for named remote hosts reachable over SSH / Telnet / a serial
   * link. The "awaiting trust" stat + per-card host-key badge drive the
   * accept-and-pin flow: until the operator pins the presented host key
   * (via Test connection), the terminal and file browser refuse to open.
   */
  import { onMount } from 'svelte';
  import {
    Server,
    Terminal,
    Cable,
    Wifi,
    Monitor,
    Plus,
    RefreshCw,
    Loader2,
    Trash2,
    PlugZap,
  } from '@lucide/svelte';
  import PageHeader from '$lib/components/PageHeader.svelte';
  import StatCard from '$lib/components/StatCard.svelte';
  import EmptyState from '$lib/components/EmptyState.svelte';
  import SearchInput from '$lib/components/SearchInput.svelte';
  import Alert from '$lib/components/Alert.svelte';
  import ConfirmDialog from '$lib/components/ConfirmDialog.svelte';
  import CardGridSkeleton from '$lib/components/CardGridSkeleton.svelte';
  import HostKeyDialog from '$lib/components/machine/HostKeyDialog.svelte';
  import { api, auth } from '$lib/stores/auth.svelte.js';
  import { navigate } from '$lib/router.svelte.js';
  import { toast } from '$lib/components/ui/toast';
  import { t } from '$lib/i18n.svelte.js';
  import { Button } from '$lib/components/ui/button';

  let machines = $state([]);
  let loading = $state(true);
  let error = $state('');
  let refreshing = $state(false);

  let search = $state('');
  let protocolFilter = $state('all');

  // Card quick-action menu ("⋯" button).
  let menuFor = $state(null);
  let testBusy = $state(''); // machine id while a test is in flight

  // Host-key trust dialog state (untrusted or changed key).
  let hkOpen = $state(false);
  let hkMachine = $state(null);
  let hkPresented = $state('');
  let hkPinned = $state('');

  // Delete confirmation.
  let deletePending = $state(null); // machine being deleted
  let deleting = $state(false);

  const PROTOCOL_ICONS = {
    ssh: Server,
    telnet: Terminal,
    serial: Cable,
    mosh: Wifi,
    local: Monitor,
  };
  function protocolLabel(p) {
    return (
      {
        ssh: t('machines.protocolSsh'),
        telnet: t('machines.protocolTelnet'),
        serial: t('machines.protocolSerial'),
        mosh: t('machines.protocolMosh'),
        local: t('machines.protocolLocal'),
      }[p] || p
    );
  }

  const stats = $derived.by(() => ({
    total: machines.length,
    ssh: machines.filter((m) => m.protocol === 'ssh').length,
    telnet: machines.filter((m) => m.protocol === 'telnet').length,
    serial: machines.filter((m) => m.protocol === 'serial').length,
    // The built-in host entry is a machine too, so it needs its own
    // bucket: without it the header reads "2 · 1 SSH · 0 Telnet · 0
    // Serial" and the numbers stop adding up.
    local: machines.filter((m) => m.protocol === 'local').length,
    untrusted: machines.filter((m) => m.protocol === 'ssh' && !m.host_key).length,
  }));

  const filtered = $derived.by(() => {
    let out = machines;
    if (protocolFilter !== 'all') out = out.filter((m) => m.protocol === protocolFilter);
    const q = search.trim().toLowerCase();
    if (q) {
      out = out.filter(
        (m) =>
          m.name.toLowerCase().includes(q) ||
          m.host.toLowerCase().includes(q) ||
          (m.user && m.user.toLowerCase().includes(q))
      );
    }
    return out;
  });

  function targetLabel(m) {
    if (m.protocol === 'serial') return m.host;
    return m.port ? `${m.host}:${m.port}` : m.host;
  }

  async function load() {
    try {
      machines = await api.listMachines();
      error = '';
    } catch (e) {
      error = e.message;
    } finally {
      loading = false;
    }
  }

  async function refresh() {
    if (refreshing) return;
    refreshing = true;
    try {
      machines = await api.listMachines();
      error = '';
    } catch (e) {
      toast.error(e.message);
    } finally {
      refreshing = false;
    }
  }

  function toggleMenu(id) {
    menuFor = menuFor === id ? null : id;
  }

  // Test connection — the entry point into the trust flow for a machine
  // that is not pinned yet, and a plain reachability check afterwards.
  async function testMachine(m) {
    const key = m.id;
    if (testBusy === key) return;
    testBusy = key;
    menuFor = null;
    try {
      const res = await api.testMachine(m.id);
      if (res.ok && res.status === 'connected') {
        toast.success(
          res.latency_ms != null
            ? `${t('machines.testConnected')} (${t('machines.testLatency', { ms: res.latency_ms })})`
            : t('machines.testConnected')
        );
      } else if (res.status === 'hostkey_untrusted' || res.status === 'hostkey_changed') {
        hkMachine = m;
        hkPresented = res.fingerprint || '';
        hkPinned = res.pinned || '';
        hkOpen = true;
      } else {
        toast.error(res.message || res.status || 'test failed');
      }
    } catch (e) {
      // 409 (hostkey_changed) and 501 (mosh) arrive as thrown ApiErrors
      // whose data is the machine-readable body.
      const d = e.data || {};
      if (d.status === 'hostkey_untrusted' || d.status === 'hostkey_changed') {
        hkMachine = m;
        hkPresented = d.fingerprint || '';
        hkPinned = d.pinned || m.host_key || '';
        hkOpen = true;
      } else {
        toast.error(e.message);
      }
    } finally {
      testBusy = '';
    }
  }

  async function confirmDelete() {
    if (!deletePending || deleting) return;
    deleting = true;
    try {
      await api.deleteMachine(deletePending.id);
      machines = machines.filter((m) => m.id !== deletePending.id);
      toast.success(t('machines.deletedOk'));
      deletePending = null;
    } catch (e) {
      toast.error(e.message);
    } finally {
      deleting = false;
    }
  }

  onMount(load);
</script>

<svelte:window onclick={() => (menuFor = null)} />

<div class="p-3 sm:p-5 w-full max-w-[1800px] mx-auto">
  <PageHeader
    title={t('machines.title')}
    subtitle={loading
      ? ''
      : `${stats.total} · ${stats.ssh} ${t('machines.protocolSsh')} · ${stats.telnet} ${t('machines.protocolTelnet')} · ${stats.serial} ${t('machines.protocolSerial')} · ${stats.local} ${t('machines.protocolLocal')}`}
  >
    {#snippet actions()}
      <button
        type="button"
        onclick={refresh}
        disabled={refreshing || loading}
        class="inline-flex items-center gap-1.5 px-3 py-1.5 text-xs font-medium rounded-lg border border-border bg-card hover:bg-muted transition-colors"
        aria-label={t('common.refresh') || 'Refresh'}
      >
        <RefreshCw class="w-3.5 h-3.5 {refreshing ? 'animate-spin' : ''}" />
        {#if !loading}
          <span class="hidden sm:inline">{t('common.refresh') || 'Refresh'}</span>
        {/if}
      </button>
      {#if auth.canMutate()}
        <Button size="sm" onclick={() => navigate('/machines/new')}>
          <Plus class="w-3.5 h-3.5 mr-1.5" />
          {t('machines.create')}
        </Button>
      {/if}
    {/snippet}
  </PageHeader>

  <div class="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-6 gap-3 mb-4">
    <StatCard
      label={t('machines.statTotal')}
      value={loading ? '…' : String(stats.total)}
      status="info"
    />
    <StatCard
      label={t('machines.statSsh')}
      value={loading ? '…' : String(stats.ssh)}
      status="info"
    />
    <StatCard
      label={t('machines.statTelnet')}
      value={loading ? '…' : String(stats.telnet)}
      status="info"
    />
    <StatCard
      label={t('machines.statSerial')}
      value={loading ? '…' : String(stats.serial)}
      status="info"
    />
    <StatCard
      label={t('machines.protocolLocal')}
      value={loading ? '…' : String(stats.local)}
      status="info"
    />
    <StatCard
      label={t('machines.statUntrusted')}
      value={loading ? '…' : String(stats.untrusted)}
      status={stats.untrusted > 0 ? 'crashed' : 'info'}
      hint={stats.untrusted > 0 ? t('machines.hostKeyUnpinned') : ''}
    />
  </div>

  {#if error}
    <Alert variant="error" class="mb-4">{error}</Alert>
  {/if}

  {#if loading}
    <CardGridSkeleton count={6} thumbnail={false} />
  {:else if machines.length === 0}
    <EmptyState icon="server" title={t('machines.empty')} description={t('machines.emptyDesc')}>
      {#snippet action()}
        {#if auth.canMutate()}
          <Button size="sm" onclick={() => navigate('/machines/new')}>
            <Plus class="w-3.5 h-3.5 mr-1.5" />
            {t('machines.addFirst')}
          </Button>
        {/if}
      {/snippet}
    </EmptyState>
  {:else}
    <div class="flex flex-wrap items-center gap-2 mb-4">
      <div class="w-full sm:w-72">
        <SearchInput bind:value={search} placeholder={t('machines.searchPlaceholder')} />
      </div>
      <select bind:value={protocolFilter} class="input w-40" aria-label={t('machines.protocol')}>
        <option value="all">{t('machines.allProtocols')}</option>
        <option value="ssh">{t('machines.protocolSsh')}</option>
        <option value="telnet">{t('machines.protocolTelnet')}</option>
        <option value="serial">{t('machines.protocolSerial')}</option>
        <option value="local">{t('machines.protocolLocal')}</option>
        <option value="mosh">{t('machines.protocolMosh')}</option>
      </select>
    </div>

    {#if filtered.length === 0}
      <div class="text-sm text-muted-foreground py-10 text-center">{t('common.noResults')}</div>
    {:else}
      <div class="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4 gap-3.5">
        {#each filtered as m (m.id)}
          {@const PIcon = PROTOCOL_ICONS[m.protocol] || Server}
          {@const needsTrust = m.protocol === 'ssh' && !m.host_key}
          {@const mosh = m.protocol === 'mosh'}
          <div
            role="button"
            tabindex="0"
            onclick={() => navigate(`/machines/${m.id}`)}
            onkeydown={(e) => {
              if (e.key === 'Enter' || e.key === ' ') {
                e.preventDefault();
                navigate(`/machines/${m.id}`);
              }
            }}
            class="group relative text-left border rounded-lg bg-card transition-[border-color,box-shadow,transform] duration-200 ease-out {menuFor ===
            m.id
              ? 'z-50 ring-2 ring-accent/30 shadow-xl'
              : 'z-0'} border-border hover:border-border-hover hover:-translate-y-0.5 hover:shadow-md"
          >
            <div class="p-3.5 space-y-2.5">
              <!-- Name + quick actions -->
              <div class="flex items-start gap-2.5 min-w-0">
                <div
                  class="w-9 h-9 rounded-lg bg-gradient-to-br from-accent/20 via-muted to-muted border border-border flex items-center justify-center text-accent shrink-0"
                >
                  <PIcon size={18} />
                </div>
                <div class="min-w-0 flex-1">
                  <div
                    class="text-sm font-semibold truncate group-hover:text-accent transition-colors"
                  >
                    {m.name}
                  </div>
                  <div
                    class="text-xs text-muted-foreground font-mono truncate"
                    title={targetLabel(m)}
                  >
                    {targetLabel(m)}
                    {#if m.protocol !== 'serial' && m.user}
                      <span class="text-muted-foreground/60"> · {m.user}</span>
                    {/if}
                  </div>
                </div>
                <div class="relative shrink-0">
                  {#if !m.builtin}
                    <button
                      type="button"
                      aria-label={t('common.actions')}
                      class="p-1 rounded-md text-muted-foreground hover:text-foreground hover:bg-muted transition-colors {menuFor ===
                      m.id
                        ? 'bg-muted text-foreground'
                        : ''}"
                      onclick={(e) => {
                        e.stopPropagation();
                        toggleMenu(m.id);
                      }}
                    >
                      <svg
                        class="w-4 h-4"
                        fill="currentColor"
                        viewBox="0 0 24 24"
                        aria-hidden="true"
                      >
                        <circle cx="5" cy="12" r="1.6" />
                        <circle cx="12" cy="12" r="1.6" />
                        <circle cx="19" cy="12" r="1.6" />
                      </svg>
                    </button>
                  {/if}
                  {#if menuFor === m.id}
                    <div
                      role="menu"
                      tabindex="-1"
                      class="absolute right-0 top-full z-50 mt-1 w-52 rounded-xl border border-border bg-popover text-popover-foreground shadow-2xl p-1 backdrop-blur-md"
                      onclick={(e) => e.stopPropagation()}
                      onkeydown={(e) => e.stopPropagation()}
                    >
                      {#if auth.canMutate()}
                        <button
                          type="button"
                          class="w-full text-left text-sm px-2 py-1.5 rounded hover:bg-muted flex items-center gap-2"
                          disabled={testBusy === m.id}
                          onclick={() => testMachine(m)}
                        >
                          {#if testBusy === m.id}
                            <Loader2 class="w-3.5 h-3.5 animate-spin" />
                          {:else}
                            <PlugZap class="w-3.5 h-3.5" />
                          {/if}
                          {testBusy === m.id ? t('machines.testing') : t('machines.test')}
                        </button>
                        <button
                          type="button"
                          class="w-full text-left text-sm px-2 py-1.5 rounded hover:bg-muted flex items-center gap-2 text-destructive"
                          onclick={() => {
                            deletePending = m;
                            menuFor = null;
                          }}
                        >
                          <Trash2 class="w-3.5 h-3.5" />
                          {t('machines.delete')}
                        </button>
                      {/if}
                    </div>
                  {/if}
                </div>
              </div>

              <!-- Badges -->
              <div class="flex flex-wrap items-center gap-1.5 pt-1 border-t border-border/50">
                <span
                  class="inline-flex items-center gap-1 text-[10px] uppercase tracking-wider px-1.5 py-0.5 rounded bg-muted border border-border text-muted-foreground"
                >
                  <PIcon class="w-3 h-3" />
                  {protocolLabel(m.protocol)}
                </span>
                {#if m.builtin}
                  <span
                    class="inline-flex items-center text-[10px] px-1.5 py-0.5 rounded bg-accent/10 border border-accent/20 text-accent font-medium"
                  >
                    {t('machines.badgeBuiltin')}
                  </span>
                {/if}
                {#if needsTrust}
                  <span
                    class="inline-flex items-center gap-1 text-[10px] px-1.5 py-0.5 rounded bg-warning/10 border border-warning/30 text-warning font-medium"
                    title={t('machines.hostKeyUnpinned')}
                  >
                    ⚠ {t('machines.hostKeyUnpinned')}
                  </span>
                {:else if m.protocol === 'ssh'}
                  <span
                    class="inline-flex items-center gap-1 text-[10px] px-1.5 py-0.5 rounded bg-success/10 border border-success/30 text-success font-medium"
                    title={m.host_key}
                  >
                    ✓ {t('machines.hostKeyPinned')}
                  </span>
                {/if}
                {#if mosh}
                  <span
                    class="inline-flex items-center gap-1 text-[10px] px-1.5 py-0.5 rounded bg-destructive/10 border border-destructive/30 text-destructive font-medium"
                    title={t('machines.moshUnavailable')}
                  >
                    {t('common.disabled')}
                  </span>
                {/if}
                {#if m.has_password || m.has_key}
                  <span class="text-[10px] text-muted-foreground/70">
                    {m.has_password ? t('machines.hasPassword') : ''}
                    {m.has_password && m.has_key ? ' · ' : ''}
                    {m.has_key ? t('machines.hasKey') : ''}
                  </span>
                {/if}
              </div>
            </div>
          </div>
        {/each}
      </div>
    {/if}
  {/if}

  <HostKeyDialog
    bind:open={hkOpen}
    machineId={hkMachine?.id || ''}
    machineName={hkMachine?.name || ''}
    presented={hkPresented}
    pinned={hkPinned}
    onPinned={(updated) => {
      const i = machines.findIndex((x) => x.id === updated?.id);
      if (i >= 0) machines[i] = updated;
    }}
  />

  <ConfirmDialog
    open={!!deletePending}
    title={deletePending ? t('machines.deleteConfirmTitle', { name: deletePending.name }) : ''}
    description={t('machines.deleteConfirmDesc')}
    confirmLabel={t('machines.delete')}
    variant="destructive"
    loading={deleting}
    onConfirm={confirmDelete}
    onCancel={() => (deletePending = null)}
  />
</div>
