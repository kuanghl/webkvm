<script>
  /**
   * HostKeyDialog — the SSH host-key trust flow (accept-and-pin).
   *
   * Shown when a machine dial or SFTP session reports an untrusted or
   * changed host key (backend test/sftp endpoints surface this as
   * status "hostkey_untrusted" / "hostkey_changed" with the presented
   * fingerprint). The operator reviews the fingerprint and pins it;
   * dials are strict after that, so a future key change is refused
   * rather than silently accepted.
   *
   *   <HostKeyDialog
   *     bind:open={hkOpen}
   *     machineId={m.id}
   *     machineName={m.name}
   *     presented={hkFingerprint}
   *     pinned={m.host_key}
   *     onPinned={(updated) => { machine = updated; }}
   *   />
   */
  import * as Dialog from '$lib/components/ui/dialog';
  import { Button } from '$lib/components/ui/button';
  import { Loader2, Copy, Check, ShieldAlert } from '@lucide/svelte';
  import { api } from '$lib/stores/auth.svelte.js';
  import { toast } from '$lib/components/ui/toast';
  import { t } from '$lib/i18n.svelte.js';

  let {
    open = $bindable(false),
    machineId,
    machineName = '',
    presented = '',
    pinned = '',
    onPinned = () => {},
  } = $props();

  let busy = $state(false);
  let copied = $state(false);
  let error = $state('');

  const isRepin = $derived(!!pinned);

  function copyFingerprint() {
    navigator.clipboard
      .writeText(presented)
      .then(() => {
        copied = true;
        setTimeout(() => (copied = false), 1800);
      })
      .catch(() => {
        /* clipboard rejected — the fingerprint is still on screen */
      });
  }

  async function pin() {
    if (busy || !presented) return;
    busy = true;
    error = '';
    try {
      const updated = await api.acceptMachineHostKey(machineId, presented);
      toast.success(t('machines.pinnedOk'));
      open = false;
      if (updated && typeof onPinned === 'function') onPinned(updated);
    } catch (e) {
      error = e.message;
    } finally {
      busy = false;
    }
  }
</script>

<Dialog.Root bind:open>
  <Dialog.Content class="sm:max-w-lg [&>*]:min-w-0">
    <Dialog.Header>
      <Dialog.Title class="flex items-center gap-2">
        <ShieldAlert class="w-4 h-4 text-warning shrink-0" />
        {isRepin ? t('machines.hostkeyChangedTitle') : t('machines.hostkeyNotPinnedTitle')}
      </Dialog.Title>
      {#if machineName}
        <Dialog.Description class="font-mono">{machineName}</Dialog.Description>
      {/if}
    </Dialog.Header>

    <div class="space-y-3">
      <p class="text-sm text-muted-foreground">
        {isRepin ? t('machines.hostkeyChangedDesc') : t('machines.hostkeyNotPinnedDesc')}
      </p>

      {#if pinned}
        <div class="rounded-md border border-border bg-muted/40 px-3 py-2">
          <div class="text-xs text-muted-foreground mb-1">{t('machines.hostkeyPinnedLabel')}</div>
          <div class="font-mono text-xs break-all text-muted-foreground line-through">{pinned}</div>
        </div>
      {/if}

      <div class="rounded-md border border-warning/30 bg-warning/5 px-3 py-2">
        <div class="text-xs text-muted-foreground mb-1 flex items-center justify-between gap-2">
          <span>{t('machines.hostkeyPresented')}</span>
          <button
            type="button"
            class="inline-flex items-center gap-1 text-xs text-foreground hover:text-accent transition-colors"
            onclick={copyFingerprint}
            title={t('common.copy') || 'Copy'}
          >
            {#if copied}
              <Check class="w-3 h-3 text-success" />
            {:else}
              <Copy class="w-3 h-3" />
            {/if}
          </button>
        </div>
        <div class="font-mono text-xs break-all">{presented}</div>
      </div>

      {#if error}
        <div class="text-sm text-destructive break-words">{error}</div>
      {/if}
    </div>

    <Dialog.Footer class="gap-2">
      <Button variant="outline" onclick={() => (open = false)} disabled={busy}>
        {t('common.cancel')}
      </Button>
      <Button onclick={pin} disabled={busy || !presented}>
        {#if busy}
          <Loader2 class="h-3.5 w-3.5 animate-spin" />
        {/if}
        {busy ? t('machines.pinning') : isRepin ? t('machines.rePin') : t('machines.pin')}
      </Button>
    </Dialog.Footer>
  </Dialog.Content>
</Dialog.Root>
