<script>
  /**
   * SftpBrowser — the machine detail Files tab.
   *
   * It used to be a single-pane browser with its own queue. The workspace
   * now owns all of that (including the 409 → host-key flow), and this
   * shell only keeps the tab's props so MachineDetail never has to know
   * which component answers them.
   */
  import FilesWorkspace from '$lib/components/machine/files/FilesWorkspace.svelte';

  let { machineId } = $props();
</script>

{#if machineId}
  <div class="flex h-[calc(100vh-320px)] min-h-0 w-full flex-col">
    <FilesWorkspace {machineId} />
  </div>
{/if}
