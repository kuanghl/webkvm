<script>
  /**
   * MachineCreate — add a new machine (SSH / Telnet / Serial / Mosh).
   *
   * The protocol drives the whole form: serial swaps host→device path and
   * port→baud rate, telnet/serial drop the credential requirements that ssh
   * enforces, and mosh is selectable (so a record can be pre-configured)
   * but flagged as unavailable in this build — the terminal will refuse
   * to open one.
   */
  import { Button } from '$lib/components/ui/button';
  import { Card, CardContent } from '$lib/components/ui/card';
  import { Input } from '$lib/components/ui/input';
  import { Label } from '$lib/components/ui/label';
  import Alert from '$lib/components/Alert.svelte';
  import { Loader2, ArrowLeft, Save } from '@lucide/svelte';
  import { api } from '$lib/stores/auth.svelte.js';
  import { navigate } from '$lib/router.svelte.js';
  import { toast } from '$lib/components/ui/toast';
  import { t } from '$lib/i18n.svelte.js';

  const PROTOCOLS = ['ssh', 'telnet', 'serial', 'mosh'];
  const BAUD_RATES = [1200, 2400, 4800, 9600, 19200, 38400, 57600, 115200];
  const DEFAULT_PORTS = { ssh: 22, telnet: 23, mosh: 6000 };

  let name = $state('');
  let protocol = $state('ssh');
  let host = $state('');
  let port = $state(String(DEFAULT_PORTS.ssh));
  let user = $state('');
  let authType = $state('password');
  let password = $state('');
  let sshKey = $state('');
  let sshKeyPassphrase = $state('');
  let notes = $state('');

  let error = $state('');
  let saving = $state(false);

  const isSerial = $derived(protocol === 'serial');
  const isMosh = $derived(protocol === 'mosh');
  const showUser = $derived(!isSerial);

  function switchProtocol(p) {
    protocol = p;
    // Re-seed the port field with the protocol's default — a stale
    // 115200 baud left over from a serial machine would 400 as a port.
    if (p === 'serial') {
      port = '115200';
    } else if (DEFAULT_PORTS[p]) {
      port = String(DEFAULT_PORTS[p]);
    }
    if (p === 'serial') {
      authType = 'none';
    } else if (p === 'ssh' && authType === 'none') {
      authType = 'password';
    }
  }

  function validate() {
    if (!name.trim()) return t('machines.sftpNameRequired');
    if (!host.trim()) return t('machines.host');
    if (!isSerial) {
      const n = parseInt(port, 10);
      if (!Number.isFinite(n) || n < 1 || n > 65535) return t('machines.port');
    }
    if (protocol === 'ssh' && !user.trim()) return t('machines.user');
    if (protocol === 'ssh' && authType === 'none') return t('machines.authType');
    return '';
  }

  async function submit(e) {
    e.preventDefault();
    if (saving) return;
    error = '';
    const v = validate();
    if (v) {
      error = v;
      return;
    }
    saving = true;
    try {
      const body = {
        name: name.trim(),
        protocol,
        host: host.trim(),
        port: parseInt(port, 10) || 0,
        user: user.trim(),
        auth_type: isSerial ? 'none' : authType,
        notes: notes.trim(),
        // An all-empty secret object means "none" on the backend.
        secret: {
          password: protocol === 'serial' ? '' : password,
          ssh_key: protocol === 'serial' ? '' : sshKey,
          ssh_key_passphrase: protocol === 'serial' ? '' : sshKeyPassphrase,
        },
      };
      const m = await api.createMachine(body);
      toast.success(t('machines.createdOk'));
      // Straight to the detail page: for an ssh machine the first
      // action there is the host-key trust flow.
      navigate(`/machines/${m.id}`);
    } catch (err) {
      error = err.message;
    } finally {
      saving = false;
    }
  }
</script>

<div class="p-3 sm:p-5 w-full max-w-3xl mx-auto">
  <div class="flex items-center gap-3 mb-6">
    <button
      onclick={() => navigate('/machines')}
      class="p-1.5 rounded-md hover:bg-muted text-muted-foreground hover:text-foreground transition-colors shrink-0"
      aria-label={t('common.back')}
    >
      <ArrowLeft class="w-4 h-4" />
    </button>
    <h1 class="text-xl font-semibold tracking-tight">{t('machines.create')}</h1>
  </div>

  {#if error}
    <Alert variant="error" class="mb-4">{error}</Alert>
  {/if}

  {#if isMosh}
    <Alert variant="warning" class="mb-4" title={t('machines.protocolMosh')}>
      {t('machines.protocolMoshHint')}
    </Alert>
  {/if}

  <form onsubmit={submit}>
    <Card>
      <CardContent class="p-5 space-y-5">
        <!-- Name -->
        <div class="space-y-1.5">
          <Label for="mc-name">{t('machines.name')} *</Label>
          <Input
            id="mc-name"
            bind:value={name}
            placeholder={t('machines.namePlaceholder')}
            class="w-full"
            maxlength="120"
          />
        </div>

        <!-- Protocol -->
        <div class="space-y-1.5">
          <Label for="mc-protocol">{t('machines.protocol')}</Label>
          <div class="grid grid-cols-2 sm:grid-cols-4 gap-2">
            {#each PROTOCOLS as p (p)}
              {@const pLabel = {
                ssh: t('machines.protocolSsh'),
                telnet: t('machines.protocolTelnet'),
                serial: t('machines.protocolSerial'),
                mosh: t('machines.protocolMosh'),
              }[p]}
              <button
                type="button"
                onclick={() => switchProtocol(p)}
                class="px-3 py-2 text-sm font-medium rounded-lg border transition-colors {protocol ===
                p
                  ? 'border-accent bg-accent/10 text-accent'
                  : 'border-border bg-card hover:bg-muted'}"
                aria-pressed={protocol === p}
              >
                {pLabel}
              </button>
            {/each}
          </div>
        </div>

        <!-- Target: host + port, or device + baud for serial -->
        <div class="grid grid-cols-1 sm:grid-cols-[1fr_160px] gap-4">
          <div class="space-y-1.5">
            <Label for="mc-host">{isSerial ? t('machines.hostSerial') : t('machines.host')}</Label>
            <Input
              id="mc-host"
              bind:value={host}
              placeholder={isSerial
                ? t('machines.hostSerialPlaceholder')
                : t('machines.hostPlaceholder')}
              class="w-full font-mono"
            />
            {#if isSerial}
              <p class="text-[11px] text-muted-foreground">
                <code class="font-mono">/dev/ttyUSB0</code> · <code class="font-mono">COM1</code>
              </p>
            {/if}
          </div>
          <div class="space-y-1.5">
            <Label for="mc-port">{isSerial ? t('machines.portSerial') : t('machines.port')}</Label>
            {#if isSerial}
              <select bind:value={port} class="input w-full tnum">
                {#each BAUD_RATES as b (b)}
                  <option value={String(b)}>{b}</option>
                {/each}
              </select>
            {:else}
              <Input
                id="mc-port"
                bind:value={port}
                type="number"
                min="1"
                max="65535"
                class="w-full tnum"
              />
            {/if}
          </div>
        </div>

        <!-- User (serial has none) -->
        {#if showUser}
          <div class="space-y-1.5">
            <Label for="mc-user">{t('machines.user')}</Label>
            <Input
              id="mc-user"
              bind:value={user}
              placeholder={t('machines.userPlaceholder')}
              class="w-full"
            />
            {#if protocol === 'telnet'}
              <p class="text-[11px] text-muted-foreground">
                {t('machines.authNone')}
                {t('common.optional').toLowerCase()}
              </p>
            {/if}
          </div>
        {/if}

        <!-- Auth type + credentials (no auth on serial) -->
        {#if !isSerial}
          <div class="space-y-1.5">
            <Label for="mc-auth">{t('machines.authType')}</Label>
            <div class="flex flex-wrap gap-2">
              {#each protocol === 'ssh' ? ['password', 'key'] : ['password', 'key', 'none'] as a (a)}
                {@const aLabel = {
                  password: t('machines.authPassword'),
                  key: t('machines.authKey'),
                  none: t('machines.authNone'),
                }[a]}
                <button
                  type="button"
                  onclick={() => (authType = a)}
                  class="px-3 py-1.5 text-xs font-medium rounded-md border transition-colors {authType ===
                  a
                    ? 'border-accent bg-accent/10 text-accent'
                    : 'border-border bg-card hover:bg-muted'}"
                  aria-pressed={authType === a}
                >
                  {aLabel}
                </button>
              {/each}
            </div>
          </div>

          {#if authType === 'password'}
            <div class="space-y-1.5">
              <Label for="mc-password">{t('machines.password')}</Label>
              <Input
                id="mc-password"
                bind:value={password}
                type="password"
                placeholder={t('machines.passwordPlaceholder')}
                class="w-full"
              />
            </div>
          {/if}

          {#if authType === 'key'}
            <div class="space-y-1.5">
              <Label for="mc-key">{t('machines.sshKey')}</Label>
              <textarea
                id="mc-key"
                bind:value={sshKey}
                placeholder={t('machines.sshKeyPlaceholder')}
                rows="5"
                class="input w-full font-mono text-xs resize-y"
              ></textarea>
            </div>
            <div class="space-y-1.5">
              <Label for="mc-keypass">{t('machines.sshKeyPassphrase')}</Label>
              <Input
                id="mc-keypass"
                bind:value={sshKeyPassphrase}
                type="password"
                placeholder={t('machines.sshKeyPassphrasePlaceholder')}
                class="w-full"
              />
            </div>
          {/if}
        {/if}

        <!-- Notes -->
        <div class="space-y-1.5">
          <Label for="mc-notes">{t('machines.notes')}</Label>
          <textarea
            id="mc-notes"
            bind:value={notes}
            placeholder={t('machines.notesPlaceholder')}
            rows="2"
            class="input w-full resize-y"
          ></textarea>
        </div>

        <div class="flex items-center justify-end gap-2 pt-2 border-t border-border/50">
          <Button
            type="button"
            variant="outline"
            onclick={() => navigate('/machines')}
            disabled={saving}
          >
            {t('common.cancel')}
          </Button>
          <Button type="submit" disabled={saving}>
            {#if saving}
              <Loader2 class="h-3.5 w-3.5 animate-spin" />
            {:else}
              <Save class="h-3.5 w-3.5 mr-1.5" />
            {/if}
            {t('common.create')}
          </Button>
        </div>
      </CardContent>
    </Card>
  </form>
</div>
