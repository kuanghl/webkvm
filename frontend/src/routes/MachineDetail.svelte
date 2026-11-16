<script>
  /**
   * MachineDetail — one remote host.
   *
   * Tabs: overview / terminal / files (ssh only) / settings. A panel
   * mounts the first time its tab is opened and then stays mounted
   * (CSS-hidden), so visiting the page never dials a terminal nobody
   * asked for, while an opened one survives a tab switch.
   *
   * Trust gate: ssh machines without a pinned host key show the
   * accept-and-pin flow instead of the terminal / file browser — dials
   * are strict, so nothing can connect before the operator has reviewed
   * the presented key.
   */
  import { Button } from '$lib/components/ui/button';
  import { Card, CardContent } from '$lib/components/ui/card';
  import { Input } from '$lib/components/ui/input';
  import { Label } from '$lib/components/ui/label';
  import Alert from '$lib/components/Alert.svelte';
  import Spinner from '$lib/components/Spinner.svelte';
  import ConfirmDialog from '$lib/components/ConfirmDialog.svelte';
  import Tabs from '$lib/components/Tabs.svelte';
  import HostKeyDialog from '$lib/components/machine/HostKeyDialog.svelte';
  import SftpBrowser from '$lib/components/machine/SftpBrowser.svelte';
  import TerminalPanel from '$lib/components/TerminalPanel.svelte';
  import {
    Server,
    Terminal,
    Cable,
    Wifi,
    Monitor,
    ArrowLeft,
    PlugZap,
    Trash2,
    Loader2,
    Save,
  } from '@lucide/svelte';
  import { api, auth } from '$lib/stores/auth.svelte.js';
  import { SvelteSet } from 'svelte/reactivity';
  import { getRoute, navigate } from '$lib/router.svelte.js';
  import { toast } from '$lib/components/ui/toast';
  import { t } from '$lib/i18n.svelte.js';
  import { carriesFiles } from '$lib/utils/fileTransfer.js';

  // App.svelte pins machineId="host" for the persistent built-in host
  // terminal instance; every other visit resolves it from the route.
  let { machineId: idProp = null } = $props();

  const route = $derived(getRoute());
  const machineId = $derived(idProp || route.params?.id || '');
  const isBuiltin = $derived(machineId === 'host');
  // The host card's settings are admin-only: the stored credential is a
  // shell on the machine webkvm itself runs on, and the backend answers
  // the card's writable surface 404 to anyone else.
  const settingsVisible = $derived(isBuiltin ? auth.isAdmin() : true);

  let machine = $state(null);
  let loading = $state(true);
  let loadError = $state('');

  let activeSection = $state('overview');

  // Lazy-first-then-keep. TerminalPanel connects its WebSocket in
  // onMount and SftpBrowser starts browsing on mount, so a panel that is
  // always mounted opens a remote session for a tab nobody clicked.
  // `overview` is the tab the page lands on, hence its seed.
  const activated = new SvelteSet(['overview']);
  $effect(() => {
    activated.add(activeSection);
  });

  // `CarriesFiles()` on the server decides who gets the file plane; the
  // tab has to ask the same question or it offers a route that 400s.
  const browsesFiles = $derived(carriesFiles(machine));

  const sectionTabs = $derived(
    [
      { id: 'overview', label: t('machines.overview') },
      { id: 'terminal', label: t('machines.terminal') },
      ...(browsesFiles ? [{ id: 'sftp', label: t('machines.sftp') }] : []),
      ...(settingsVisible ? [{ id: 'settings', label: t('machines.settings') }] : []),
    ].filter(Boolean)
  );

  const PROTOCOL_ICONS = {
    ssh: Server,
    telnet: Terminal,
    serial: Cable,
    mosh: Wifi,
    local: Monitor,
  };

  function targetLabel(m) {
    if (m.protocol === 'serial') return m.host;
    return m.port ? `${m.host}:${m.port}` : m.host;
  }

  // Key-fragment for the dynamic t() lookups below: a record without
  // protocol/auth_type must not blank the page on .charAt of undefined.
  function cap(v) {
    return v ? v.charAt(0).toUpperCase() + v.slice(1) : '';
  }

  function fmtDate(iso) {
    if (!iso) return '—';
    try {
      return new Date(iso).toLocaleString();
    } catch {
      return iso;
    }
  }

  // ---- load ----
  async function load() {
    if (!machineId) return;
    loading = true;
    loadError = '';
    try {
      machine = await api.getMachine(machineId);
    } catch (e) {
      // A rejected/absent record must not escape as an unhandled
      // rejection — the error branch below renders it instead.
      loadError = (e && e.message) || String(e);
    } finally {
      loading = false;
    }
  }
  $effect(() => {
    // Re-load when navigating between machines without a full page reload.
    const id = machineId;
    if (id) {
      machine = null;
      load();
    }
  });

  // ---- connection test / host-key trust flow ----
  let testBusy = $state(false);
  let hkOpen = $state(false);
  let hkPresented = $state('');
  let hkPinned = $state('');

  async function runTest() {
    if (testBusy || !machine) return;
    testBusy = true;
    try {
      const res = await api.testMachine(machine.id);
      if (res.ok && res.status === 'connected') {
        toast.success(
          res.latency_ms != null
            ? `${t('machines.testConnected')} (${t('machines.testLatency', { ms: res.latency_ms })})`
            : t('machines.testConnected')
        );
      } else if (res.status === 'hostkey_untrusted' || res.status === 'hostkey_changed') {
        hkPresented = res.fingerprint || '';
        hkPinned = res.pinned || machine.host_key || '';
        hkOpen = true;
      } else if (res.status === 'no_credentials') {
        toast.warning(t('machines.hostTestNoCreds'));
      } else if (res.status === 'auth_failed') {
        toast.error(t('machines.hostTestAuthFailed'));
      } else {
        toast.error(res.message || res.status || 'test failed');
      }
    } catch (e) {
      const d = e.data || {};
      if (d.status === 'hostkey_untrusted' || d.status === 'hostkey_changed') {
        hkPresented = d.fingerprint || '';
        hkPinned = d.pinned || machine.host_key || '';
        hkOpen = true;
      } else {
        toast.error(e.message);
      }
    } finally {
      testBusy = false;
    }
  }

  // ---- delete ----
  let deleteOpen = $state(false);
  let deleting = $state(false);
  async function confirmDelete() {
    if (!machine || deleting) return;
    deleting = true;
    try {
      await api.deleteMachine(machine.id);
      toast.success(t('machines.deletedOk'));
      navigate('/machines');
    } catch (e) {
      toast.error(e.message);
      deleting = false;
    }
  }

  // ---- host card settings (admin-only) ----
  let hf = $state({ user: '', authType: 'none', password: '', notes: '' });
  let hfError = $state('');
  let hfSaving = $state(false);

  async function saveHostSettings() {
    if (hfSaving || !machine) return;
    hfError = '';
    if (hf.authType === 'password' && !hf.user.trim()) {
      return (hfError = t('machines.userRequired'));
    }
    if (hf.authType === 'password' && !hf.password && !machine.has_password) {
      // Nothing stored to keep and nothing being sent.
      return (hfError = t('machines.hostCredsPasswordRequired'));
    }
    hfSaving = true;
    try {
      // A blank password sends an empty secret = "keep the current one";
      // "none" clears it on the backend.
      machine = await api.updateMachine(machine.id, {
        user: hf.user.trim(),
        auth_type: hf.authType,
        notes: hf.notes.trim(),
        secret: hf.password ? { password: hf.password } : {},
      });
      toast.success(t('machines.savedOk'));
      hf.password = ''; // never keep a typed secret in the form
    } catch (e) {
      hfError = e.message;
    } finally {
      hfSaving = false;
    }
  }

  // ---- settings ----
  const BAUD_RATES = [1200, 2400, 4800, 9600, 19200, 38400, 57600, 115200];
  const DEFAULT_PORTS = { ssh: 22, telnet: 23, mosh: 6000 };

  // Credentials are never returned by the API; blank fields mean
  // "keep the current value" (the backend treats an all-empty secret
  // object as a no-op).
  let f = $state({
    name: '',
    protocol: 'ssh',
    host: '',
    port: '',
    user: '',
    authType: 'password',
    notes: '',
    password: '',
    sshKey: '',
    sshKeyPassphrase: '',
  });
  let saveError = $state('');
  let saving = $state(false);

  // Re-seed the forms when the machine's identity changes (initial load,
  // or navigating to another machine) — never when a field of the
  // machine changes: after a save, `machine` is replaced by the server's
  // answer and the forms must keep whatever the operator is still
  // editing.
  let seededMachineId = $state('');
  $effect(() => {
    const id = machine?.id || '';
    if (!id || id === seededMachineId) return;
    seededMachineId = id;
    const m = machine;
    f = {
      name: m.name,
      protocol: m.protocol,
      host: m.host,
      port: String(m.port || ''),
      user: m.user || '',
      authType: m.auth_type || 'none',
      notes: m.notes || '',
      password: '',
      sshKey: '',
      sshKeyPassphrase: '',
    };
    hf = {
      user: m.user || '',
      authType: m.auth_type || 'none',
      password: '',
      notes: m.notes || '',
    };
    hfError = '';
    saveError = '';
  });

  function switchProtocol(p) {
    f.protocol = p;
    if (p === 'serial') {
      f.port = '115200';
      f.authType = 'none';
    } else if (DEFAULT_PORTS[p]) {
      f.port = String(DEFAULT_PORTS[p]);
    }
    if (p === 'ssh' && f.authType === 'none') f.authType = 'password';
  }

  async function saveSettings() {
    if (saving || !machine) return;
    saveError = '';
    const isSerial = f.protocol === 'serial';
    if (!f.name.trim()) return (saveError = t('machines.sftpNameRequired'));
    if (!f.host.trim()) return (saveError = t('machines.host'));
    if (!isSerial) {
      const n = parseInt(f.port, 10);
      if (!Number.isFinite(n) || n < 1 || n > 65535) return (saveError = t('machines.port'));
    }
    if (f.protocol === 'ssh' && !f.user.trim()) return (saveError = t('machines.user'));
    if (f.protocol === 'ssh' && f.authType === 'none') return (saveError = t('machines.authType'));

    saving = true;
    try {
      const body = {
        name: f.name.trim(),
        protocol: f.protocol,
        host: f.host.trim(),
        port: parseInt(f.port, 10) || 0,
        user: f.user.trim(),
        auth_type: isSerial ? 'none' : f.authType,
        notes: f.notes.trim(),
        secret: {
          password: isSerial ? '' : f.password,
          ssh_key: isSerial ? '' : f.sshKey,
          ssh_key_passphrase: isSerial ? '' : f.sshKeyPassphrase,
        },
      };
      machine = await api.updateMachine(machine.id, body);
      toast.success(t('machines.savedOk'));
    } catch (e) {
      saveError = e.message;
    } finally {
      saving = false;
    }
  }
</script>

<div class="p-3 sm:p-5 w-full max-w-[1400px] mx-auto">
  {#if loading}
    <div class="flex items-center gap-2 py-16 justify-center text-muted-foreground">
      <Spinner class="w-4 h-4" />
      <span class="text-sm">{t('common.loading')}</span>
    </div>
  {:else if loadError}
    <Alert variant="error">{loadError}</Alert>
    <div class="mt-4">
      <Button variant="outline" size="sm" onclick={() => navigate('/machines')}>
        <ArrowLeft class="w-3.5 h-3.5 mr-1.5" />
        {t('machines.title')}
      </Button>
    </div>
  {:else if machine}
    {@const PIcon = PROTOCOL_ICONS[machine.protocol] || Server}
    {@const needsTrust = machine.protocol === 'ssh' && !machine.host_key}
    <div class="flex flex-wrap items-center justify-between gap-3 mb-6">
      <div class="flex items-center gap-3 flex-wrap min-w-0 flex-1">
        <button
          onclick={() => navigate('/machines')}
          class="p-1.5 rounded-md hover:bg-muted text-muted-foreground hover:text-foreground transition-colors shrink-0"
          aria-label={t('common.back')}
        >
          <ArrowLeft class="w-4 h-4" />
        </button>
        <div
          class="w-9 h-9 rounded-lg bg-gradient-to-br from-accent/20 via-muted to-muted border border-border flex items-center justify-center text-accent shrink-0"
        >
          <PIcon size={18} />
        </div>
        <h1 class="text-xl font-semibold tracking-tight truncate min-w-0">{machine.name}</h1>
        {#if machine.builtin}
          <span
            class="inline-flex items-center text-xs px-2 py-0.5 rounded bg-muted border border-border text-muted-foreground font-medium shrink-0"
          >
            {t('machines.badgeBuiltin')}
          </span>
        {/if}
        <span
          class="inline-flex items-center gap-1 text-xs px-2 py-0.5 rounded bg-accent/10 border border-accent/20 text-accent font-mono shrink-0 max-w-[24rem]"
          title={targetLabel(machine)}
        >
          {targetLabel(machine)}
        </span>
        {#if needsTrust}
          <span
            class="inline-flex items-center gap-1 text-xs px-2 py-0.5 rounded bg-warning/10 border border-warning/30 text-warning font-medium shrink-0"
          >
            ⚠ {t('machines.hostKeyUnpinned')}
          </span>
        {:else if machine.protocol === 'ssh'}
          <span
            class="inline-flex items-center gap-1 text-xs px-2 py-0.5 rounded bg-success/10 border border-success/30 text-success font-medium shrink-0"
            title={machine.host_key}
          >
            ✓ {t('machines.hostKeyPinned')}
          </span>
        {/if}
        {#if machine.protocol === 'mosh'}
          <span
            class="inline-flex items-center gap-1 text-xs px-2 py-0.5 rounded bg-destructive/10 border border-destructive/30 text-destructive font-medium shrink-0"
            title={t('machines.moshUnavailable')}
          >
            {t('common.disabled')}
          </span>
        {/if}
      </div>
      {#if (isBuiltin && auth.isAdmin()) || (!isBuiltin && auth.canMutate())}
        <div class="flex flex-wrap items-center gap-2 min-w-0 max-w-full">
          <Button variant="outline" size="sm" onclick={runTest} disabled={testBusy}>
            {#if testBusy}
              <Loader2 class="w-3.5 h-3.5 animate-spin mr-1.5" />
            {:else}
              <PlugZap class="w-3.5 h-3.5 mr-1.5" />
            {/if}
            {testBusy ? t('machines.testing') : t('machines.test')}
          </Button>
          {#if !isBuiltin}
            <Button
              variant="outline"
              size="sm"
              class="text-destructive border-destructive/40 hover:bg-destructive/10"
              onclick={() => (deleteOpen = true)}
            >
              <Trash2 class="w-3.5 h-3.5 mr-1.5" />
              {t('machines.delete')}
            </Button>
          {/if}
        </div>
      {/if}
    </div>

    <Tabs tabs={sectionTabs} bind:active={activeSection} class="mb-1" />

    <!-- A panel mounts on its first activation and then only hides:
         tearing a live terminal down on every tab switch would be
         worse than the lazy mount that replaces it. -->
    {#if activated.has('overview')}
      <div class="space-y-5 {activeSection === 'overview' ? '' : 'hidden'}">
        <div class="grid grid-cols-1 lg:grid-cols-2 gap-4">
          <Card>
            <CardContent class="p-4">
              <div class="text-xs text-muted-foreground font-medium tracking-wide mb-3">
                {t('common.details')}
              </div>
              <dl class="space-y-2 text-sm">
                <div class="flex gap-3">
                  <dt class="w-28 shrink-0 text-muted-foreground">{t('machines.protocol')}</dt>
                  <dd>{t(`machines.protocol${cap(machine.protocol)}`) || machine.protocol}</dd>
                </div>
                {#if !machine.builtin}
                  <div class="flex gap-3">
                    <dt class="w-28 shrink-0 text-muted-foreground">
                      {machine.protocol === 'serial'
                        ? t('machines.hostSerial')
                        : t('machines.host')}
                    </dt>
                    <dd class="font-mono break-all">{targetLabel(machine)}</dd>
                  </div>
                {/if}
                {#if machine.protocol !== 'serial' && (!machine.builtin || machine.user)}
                  <div class="flex gap-3">
                    <dt class="w-28 shrink-0 text-muted-foreground">{t('machines.user')}</dt>
                    <dd>{machine.user || t('common.none')}</dd>
                  </div>
                  <div class="flex gap-3">
                    <dt class="w-28 shrink-0 text-muted-foreground">{t('machines.authType')}</dt>
                    <dd>
                      {machine.builtin
                        ? t(
                            machine.auth_type === 'password'
                              ? 'machines.hostCredsAuto'
                              : 'machines.hostCredsManual'
                          )
                        : t(`machines.auth${cap(machine.auth_type)}`) || machine.auth_type}
                    </dd>
                  </div>
                {/if}
                {#if machine.has_password}
                  <div class="flex gap-3">
                    <dt class="w-28 shrink-0 text-muted-foreground">{t('machines.hasPassword')}</dt>
                    <dd class="text-success">✓</dd>
                  </div>
                {/if}
                {#if machine.has_key}
                  <div class="flex gap-3">
                    <dt class="w-28 shrink-0 text-muted-foreground">{t('machines.hasKey')}</dt>
                    <dd class="text-success">
                      ✓
                      {#if machine.key_fingerprint}
                        <span
                          class="block font-mono text-xs text-muted-foreground mt-0.5 break-all"
                          title={t('machines.keyFingerprint')}>{machine.key_fingerprint}</span
                        >
                      {/if}
                    </dd>
                  </div>
                {/if}
                {#if machine.notes}
                  <div class="flex gap-3">
                    <dt class="w-28 shrink-0 text-muted-foreground">{t('machines.notes')}</dt>
                    <dd class="break-words">{machine.notes}</dd>
                  </div>
                {/if}
                {#if !machine.builtin}
                  <div class="flex gap-3">
                    <dt class="w-28 shrink-0 text-muted-foreground">{t('machines.created')}</dt>
                    <dd class="text-muted-foreground">{fmtDate(machine.created_at)}</dd>
                  </div>
                  <div class="flex gap-3">
                    <dt class="w-28 shrink-0 text-muted-foreground">{t('machines.updated')}</dt>
                    <dd class="text-muted-foreground">{fmtDate(machine.updated_at)}</dd>
                  </div>
                {/if}
              </dl>
            </CardContent>
          </Card>

          {#if machine.protocol === 'ssh'}
            <Card>
              <CardContent class="p-4">
                <div class="text-xs text-muted-foreground font-medium tracking-wide mb-3">
                  {t('machines.hostKey')}
                </div>
                {#if machine.host_key}
                  <div class="rounded-md border border-success/30 bg-success/5 px-3 py-2">
                    <div class="text-xs text-success mb-1">✓ {t('machines.hostKeyPinned')}</div>
                    <div class="font-mono text-xs break-all">{machine.host_key}</div>
                  </div>
                {:else}
                  <div class="rounded-md border border-warning/30 bg-warning/5 px-3 py-2">
                    <div class="text-xs text-warning mb-1">⚠ {t('machines.hostKeyUnpinned')}</div>
                    <p class="text-xs text-muted-foreground mb-2">
                      {t('machines.hostkeyNotPinnedDesc')}
                    </p>
                    {#if auth.canMutate()}
                      <Button size="sm" variant="outline" onclick={runTest} disabled={testBusy}>
                        {#if testBusy}
                          <Loader2 class="w-3.5 h-3.5 animate-spin mr-1.5" />
                        {:else}
                          <PlugZap class="w-3.5 h-3.5 mr-1.5" />
                        {/if}
                        {testBusy ? t('machines.testing') : t('machines.test')}
                      </Button>
                    {/if}
                  </div>
                {/if}
              </CardContent>
            </Card>
          {/if}
        </div>
      </div>
    {/if}

    <!-- Terminal -->
    {#if activated.has('terminal')}
      <div class="space-y-4 {activeSection === 'terminal' ? '' : 'hidden'}">
        {#if machine.protocol === 'mosh'}
          <Card>
            <CardContent class="p-5">
              <Alert variant="warning" title={t('machines.terminal')}>
                {t('machines.terminalUnavailable')}
                {t('machines.moshUnavailable')}
              </Alert>
            </CardContent>
          </Card>
        {:else if needsTrust}
          <Card>
            <CardContent class="p-5">
              <Alert variant="warning" title={t('machines.hostkeyNotPinnedTitle')} class="mb-3">
                {t('machines.terminalUnpinned')}
              </Alert>
              <div class="rounded-md border border-border bg-muted/40 px-3 py-2 mb-3">
                <div class="text-xs text-muted-foreground mb-1">{t('machines.hostKey')}</div>
                <div class="font-mono text-xs text-muted-foreground">—</div>
              </div>
              {#if auth.canMutate()}
                <Button size="sm" onclick={runTest} disabled={testBusy}>
                  {#if testBusy}
                    <Loader2 class="w-3.5 h-3.5 animate-spin mr-1.5" />
                  {:else}
                    <PlugZap class="w-3.5 h-3.5 mr-1.5" />
                  {/if}
                  {testBusy ? t('machines.testing') : t('machines.test')}
                </Button>
              {/if}
            </CardContent>
          </Card>
        {:else}
          <Card>
            <CardContent class="p-0">
              <TerminalPanel
                mode={machine.builtin ? 'host' : 'machine'}
                machineId={machine.id}
                machineProtocol={machine.protocol}
                height="calc(100vh - 320px)"
              />
            </CardContent>
          </Card>
        {/if}
      </div>
    {/if}

    <!-- Files (ssh machines and the built-in host card) -->
    {#if browsesFiles && activated.has('sftp')}
      <div class="space-y-4 {activeSection === 'sftp' ? '' : 'hidden'}">
        {#if needsTrust}
          <Card>
            <CardContent class="p-5">
              <Alert variant="warning" title={t('machines.hostkeyNotPinnedTitle')} class="mb-3">
                {t('machines.terminalUnpinned')}
              </Alert>
              {#if auth.canMutate()}
                <Button size="sm" onclick={runTest} disabled={testBusy}>
                  {#if testBusy}
                    <Loader2 class="w-3.5 h-3.5 animate-spin mr-1.5" />
                  {:else}
                    <PlugZap class="w-3.5 h-3.5 mr-1.5" />
                  {/if}
                  {testBusy ? t('machines.testing') : t('machines.test')}
                </Button>
              {/if}
            </CardContent>
          </Card>
        {:else}
          <SftpBrowser machineId={machine.id} />
        {/if}
      </div>
    {/if}

    <!-- Settings -->
    {#if activated.has('settings')}
      <div class="space-y-4 {activeSection === 'settings' ? '' : 'hidden'}">
        <!-- Unreachable today: a non-admin's sectionTabs never lists
             `settings` for the built-in card (settingsVisible), so this
             tab cannot be activated. Kept as a defensive render — a
             neutral hint card rather than a form that would 404 on
             save. -->
        {#if machine.builtin && !auth.isAdmin()}
          <Card>
            <CardContent class="p-5">
              <Alert variant="info">{t('machines.settingsSecrets')}</Alert>
            </CardContent>
          </Card>
        {:else if machine.builtin}
          {#if hfError}
            <Alert variant="error">{hfError}</Alert>
          {/if}
          <Card>
            <CardContent class="p-5 space-y-5">
              <div>
                <div class="text-sm font-medium">{t('machines.hostCredsTitle')}</div>
                <p class="text-xs text-muted-foreground mt-1">
                  {t('machines.hostCredsDesc')}
                </p>
              </div>

              <div class="grid grid-cols-1 sm:grid-cols-2 gap-4">
                <div class="space-y-1.5">
                  <Label for="md-host-user">{t('machines.user')}</Label>
                  <Input id="md-host-user" bind:value={hf.user} class="w-full" />
                </div>
                <div class="space-y-1.5">
                  <Label for="md-host-auth">{t('machines.authType')}</Label>
                  <select id="md-host-auth" bind:value={hf.authType} class="input w-full">
                    <option value="password">{t('machines.hostCredsAuto')}</option>
                    <option value="none">{t('machines.hostCredsManual')}</option>
                  </select>
                </div>
              </div>

              {#if hf.authType === 'password'}
                <div class="space-y-1.5">
                  <Label for="md-host-password">{t('machines.password')}</Label>
                  <Input
                    id="md-host-password"
                    bind:value={hf.password}
                    type="password"
                    placeholder={t('machines.passwordPlaceholder')}
                    class="w-full"
                  />
                </div>
                <p class="text-[11px] text-muted-foreground -mt-2">
                  {t('machines.settingsLeaveBlank')}
                </p>
              {/if}

              <div class="space-y-1.5">
                <Label for="md-host-notes">{t('machines.notes')}</Label>
                <textarea
                  id="md-host-notes"
                  bind:value={hf.notes}
                  rows="2"
                  class="input w-full resize-y"
                ></textarea>
              </div>

              <div class="flex justify-end pt-2 border-t border-border/50">
                <Button onclick={saveHostSettings} disabled={hfSaving}>
                  {#if hfSaving}
                    <Loader2 class="h-3.5 w-3.5 animate-spin" />
                  {:else}
                    <Save class="h-3.5 w-3.5 mr-1.5" />
                  {/if}
                  {t('machines.save')}
                </Button>
              </div>
            </CardContent>
          </Card>
        {:else if !auth.canMutate()}
          <Card>
            <CardContent class="p-5">
              <Alert variant="info">{t('machines.settingsSecrets')}</Alert>
            </CardContent>
          </Card>
        {:else}
          {#if saveError}
            <Alert variant="error">{saveError}</Alert>
          {/if}
          <Card>
            <CardContent class="p-5 space-y-5">
              <div class="grid grid-cols-1 sm:grid-cols-2 gap-4">
                <div class="space-y-1.5">
                  <Label for="md-name">{t('machines.name')}</Label>
                  <Input id="md-name" bind:value={f.name} class="w-full" maxlength="120" />
                </div>
                <div class="space-y-1.5">
                  <Label>{t('machines.protocol')}</Label>
                  <select
                    bind:value={f.protocol}
                    class="input w-full"
                    onchange={() => switchProtocol(f.protocol)}
                  >
                    <option value="ssh">{t('machines.protocolSsh')}</option>
                    <option value="telnet">{t('machines.protocolTelnet')}</option>
                    <option value="serial">{t('machines.protocolSerial')}</option>
                    <option value="mosh">{t('machines.protocolMosh')}</option>
                  </select>
                </div>
              </div>

              <div class="grid grid-cols-1 sm:grid-cols-[1fr_160px] gap-4">
                <div class="space-y-1.5">
                  <Label for="md-host"
                    >{f.protocol === 'serial'
                      ? t('machines.hostSerial')
                      : t('machines.host')}</Label
                  >
                  <Input
                    id="md-host"
                    bind:value={f.host}
                    placeholder={f.protocol === 'serial'
                      ? t('machines.hostSerialPlaceholder')
                      : t('machines.hostPlaceholder')}
                    class="w-full font-mono"
                  />
                </div>
                <div class="space-y-1.5">
                  <Label for="md-port"
                    >{f.protocol === 'serial'
                      ? t('machines.portSerial')
                      : t('machines.port')}</Label
                  >
                  {#if f.protocol === 'serial'}
                    <select bind:value={f.port} class="input w-full tnum">
                      {#each BAUD_RATES as b (b)}
                        <option value={String(b)}>{b}</option>
                      {/each}
                    </select>
                  {:else}
                    <Input
                      id="md-port"
                      bind:value={f.port}
                      type="number"
                      min="1"
                      max="65535"
                      class="w-full tnum"
                    />
                  {/if}
                </div>
              </div>

              {#if f.protocol !== 'serial'}
                <div class="grid grid-cols-1 sm:grid-cols-2 gap-4">
                  <div class="space-y-1.5">
                    <Label for="md-user">{t('machines.user')}</Label>
                    <Input id="md-user" bind:value={f.user} class="w-full" />
                  </div>
                  <div class="space-y-1.5">
                    <Label>{t('machines.authType')}</Label>
                    <select bind:value={f.authType} class="input w-full">
                      {#if f.protocol === 'ssh'}
                        <option value="password">{t('machines.authPassword')}</option>
                        <option value="key">{t('machines.authKey')}</option>
                      {:else}
                        <option value="password">{t('machines.authPassword')}</option>
                        <option value="key">{t('machines.authKey')}</option>
                        <option value="none">{t('machines.authNone')}</option>
                      {/if}
                    </select>
                  </div>
                </div>

                {#if f.authType === 'password'}
                  <div class="space-y-1.5">
                    <Label for="md-password">{t('machines.password')}</Label>
                    <Input
                      id="md-password"
                      bind:value={f.password}
                      type="password"
                      placeholder={t('machines.passwordPlaceholder')}
                      class="w-full"
                    />
                  </div>
                {/if}

                {#if f.authType === 'key'}
                  <div class="space-y-1.5">
                    <Label for="md-key">{t('machines.sshKey')}</Label>
                    <textarea
                      id="md-key"
                      bind:value={f.sshKey}
                      placeholder={t('machines.sshKeyPlaceholder')}
                      rows="5"
                      class="input w-full font-mono text-xs resize-y"
                    ></textarea>
                  </div>
                  <div class="space-y-1.5">
                    <Label for="md-keypass">{t('machines.sshKeyPassphrase')}</Label>
                    <Input
                      id="md-keypass"
                      bind:value={f.sshKeyPassphrase}
                      type="password"
                      placeholder={t('machines.sshKeyPassphrasePlaceholder')}
                      class="w-full"
                    />
                  </div>
                {/if}
              {/if}

              <p class="text-[11px] text-muted-foreground -mt-2">
                {t('machines.settingsLeaveBlank')}
              </p>

              <div class="space-y-1.5">
                <Label for="md-notes">{t('machines.notes')}</Label>
                <textarea id="md-notes" bind:value={f.notes} rows="2" class="input w-full resize-y"
                ></textarea>
              </div>

              <div class="flex justify-end pt-2 border-t border-border/50">
                <Button onclick={saveSettings} disabled={saving}>
                  {#if saving}
                    <Loader2 class="h-3.5 w-3.5 animate-spin" />
                  {:else}
                    <Save class="h-3.5 w-3.5 mr-1.5" />
                  {/if}
                  {t('machines.save')}
                </Button>
              </div>
            </CardContent>
          </Card>
        {/if}
      </div>
    {/if}

    <HostKeyDialog
      bind:open={hkOpen}
      machineId={machine.id}
      machineName={machine.name}
      presented={hkPresented}
      pinned={hkPinned}
      onPinned={(updated) => {
        if (updated?.id === machine.id) machine = updated;
      }}
    />

    <ConfirmDialog
      open={deleteOpen}
      title={t('machines.deleteConfirmTitle', { name: machine.name })}
      description={t('machines.deleteConfirmDesc')}
      confirmLabel={t('machines.delete')}
      variant="destructive"
      loading={deleting}
      onConfirm={confirmDelete}
      onCancel={() => (deleteOpen = false)}
    />
  {/if}
</div>
