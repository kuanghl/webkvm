# Changelog

All notable changes to this project are documented in this file,
following [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and [Semantic Versioning](https://semver.org/).

Spanish version: [CHANGELOG.es.md](CHANGELOG.es.md).

## [Unreleased]

### Added
- **Remote Access (machines)**: register hosts you reach over **SSH** (password or private key), **Telnet** or a **serial** line (`/dev/ttyUSB0` + baud rate) and get a browser terminal for each, plus an SFTP file browser on SSH machines; your own host is listed as the built-in **Local** machine. Credentials stay server-side in a `0600` file and are never returned to the browser, and the first SSH connection pins the host key only after you confirm its fingerprint.
- **Files workspace, now a tab on the machine**: one pane by default showing the machine you are on; the **Transfer** button opens the second pane and the copy is aimed by name instead of "the right pane". Copy folders and files **machine → machine** — streamed directly between the peers, not through the WebKVM host — or upload/download from the browser, recursively and with per-file progress. Writes are atomic (`.webkvm-part-<job>` then rename), symlinks are re-created as links instead of followed, mode and mtime are preserved, fifos/sockets/devices are skipped and counted rather than hanging the job, and a finished folder arrives as `.tar.gz`. The **Transfers** queue aggregates one row per action with speed, ETA and cancel.
- **Files on your own host**: the built-in **Local** card browses this machine's filesystem and works as both a copy source and destination. It is served by an in-process SFTP session — the same client path, the same atomic writes and the same session leases a remote machine gets — and it is admin-only, because reading and writing it runs with WebKVM's own privileges: any other role gets exactly the answer for a machine that does not exist.
- **Credentials for the Local machine**: the built-in host card now stores a login account and a password the way an SSH machine does — kept in the same `0600` secrets file under the card's id and never returned to the browser. With a pair stored, the host terminal signs itself in: it starts `login` with the stored user and types the password once at the prompt, and if the host rejects it the prompt is handed back to you exactly as before (no second guess is ever made). The card's **Settings** tab (admin only) saves the account, the mode (automatic or manual login) and notes — a blank password keeps the current one, and manual mode clears it — and a **Test** button verifies the stored pair against this machine's own login, the same bytes the terminal will auto-type. There is no host key to pin on the local card: a getty login has no key half, so that part of the SSH flow does not apply to it.
- **ZMODEM upload on Telnet and serial machines** (the `rz` direction): pick files in the terminal's ZMODEM panel and they are staged on the WebKVM host — a private directory no browser can list, discarded after 15 minutes if the terminal never claims it — then the backend plays the ZMODEM sender against the remote `rz` inside the same open terminal, one block at a time with an acknowledge per block. The command typed is `rz -q -E`, so the remote can neither ask a question nobody is watching nor overwrite: an existing name becomes `name.0`. The queue gets one ordinary upload row with live progress and cancel, and it has no result to fetch. Folders arrive flattened to their file names because `rz` never recurses. SSH machines keep their Files tab.
- **ZMODEM download on Telnet and serial machines**: the terminal runs `sz` on the remote and files the batch as an ordinary transfer job, so the queue, the progress and the download link behave exactly as over SFTP. Needs `lrzsz` on the remote; nothing is taken from the terminal stream unless you start the transfer, and if the remote stays silent the terminal is released and the keystrokes typed meanwhile are replayed untouched.
- **Files workspace: type a path, see folder sizes, choose what moves, and pause what is moving** — the four long-standing gaps in the Files tab:
  - The last crumb is now a **path bar**: type an absolute path (`/home/user`) or a name relative to the current folder and press Enter to jump. A blur that did not navigate reverts the display.
  - **Directories show a size**. A listing cannot carry one, so each folder is probed in the background with a bounded walk (depth 12, 2 000 entries, a 10 s window per request): the cell shows the total, an `≈` mark when the walk stopped early, `…` while the probe is in flight, and `—` when the folder cannot be read.
  - **Rows have checkboxes** and the header has a select-all (matching Ctrl+A). A copy or download starts on what you ticked — selection is what you can see and change, not the last focused row.
  - **Upload a folder**: a dedicated button opens the folder picker; the tree shape rides in `webkitRelativePath`, the remote skeleton is created first, and empty folders arrive.
  - **Several selected entries download as one job**: the server merges the roots into a single tree (a second `logs` lands as `logs (2)`) and streams it back as one `tar.gz` — one queue row, one browser save.
  - The **Transfers** queue can **pause** a running job: it parks between 64 KiB chunks and keeps its session alive, then **resumes** from where it stopped — or is canceled from parked. Removing a row is refused while it is paused, for the same reason it is refused while running: the worker is mid-transfer.

### Fixed
- **Upload and download do what they say**: the download button only ever produced a queue row — nothing fetched the finished job — so it looked dead. The file now reaches the browser once, exactly when the job reports done, and a queue re-read after an SSE reconnect never replays a download nobody asked for. The upload input was hidden with `display:none`, which a browser will not open on a button click; it is screen-reader-only now.
- **The Files tab follows the machine, not the moment it was rendered**: the transfer buttons were passed inside a plain `actions` object, which froze which of the two the pane showed, so a pane kept its "open transfer" or "copy to <name>" button after the second pane appeared or closed.
- The standalone `#/files` page is gone: it duplicated the machine's own Files tab behind a machine selector that had already been chosen by the page you were on.
- **Copy and folder downloads show live progress in the queue**: the row sat at `0 B / 0 B` with no bar for the whole transfer, because the server only broadcast when a job finished or failed — and a single-file copy never even knew its total. The plan is now published before the first byte (so the row has its denominator the moment the tree is walked, single file included) and a frame is broadcast at most every 300 ms while chunks stream, the same rhythm the upload/download paths already used.
- **The Transfers queue can be cleaned up**: every finished row has a trash icon that actually works in all cases — a job the server already forgot (evicted past its cap) used to fail the delete silently and a progress frame still in flight could bring the row back from the dead; the delete is now optimistic with a tombstone, a 404 counts as success, and any other failure rolls the row back and says why. The header gains a **Clear N finished** button that unloads every done/errored/canceled job in one server pass — running and paused jobs are untouched, so a clear can never stop a live transfer.
- **The destination pane refreshes itself when a job finishes**: a copy or upload landing in a folder you were looking at needed a manual reload. The workspace now watches finished jobs and re-asks the panes that own the destination — the destination itself, its ancestors, and (in the split view) the *other* pane, which is where the destination usually sits.
- **The running count and the event stream survive a disconnect**: after a single network or server hiccup, the SSE event stream used to settle into a silent loop that tore the connection down and re-established it about once a second — and every event broadcast in the gaps was lost for good. A lost "VM stopped" update is exactly what could leave the sidebar badge and the Virtual Machines "N running" count stale until a full page reload, and the constant connection churn left the interface sluggish. A replaced or closed connection can no longer schedule a new reconnect (and a live connection cancels any pending one), and every view that builds state from the stream — the running badge, the VM list, the VM detail page — re-reads its data whenever the stream (re)opens, so a missed update heals itself on the next reconnect.

## [0.1.11] — 2026-11-06

### Added
- **3D acceleration (virgl) for virtio video**, reported by [@sfs-pra](https://github.com/sfs-pra) in [#9](https://github.com/Slaker19/webkvm/issues/9): new `accel_3d` option emits `<acceleration accel3d='yes'/>` plus a validated `<graphics type='egl-headless'>` renderer, so Wayland compositors (niri, sway) start inside a guest. Fixes the underlying bug where saving the console type wiped every `<graphics>` block, silently disabling 3D. Gated on host capabilities (render node + `egl-headless`), shown disabled with the reason otherwise.
- **Updater shows tag + commit**: `#/status` now surfaces the latest release tag alongside the newest commit on `main` (`latest_commit`/`build_commit`), and source-mode update compares the built commit against `main` instead of always assuming an update exists.
- **Backup → notifications**: backup success/failure now flows through the notification channels (Telegram, Discord, Slack, webhook, etc.) via `Runner.SetAlertFunc`.
- **Docker-based 1-click apps deploy as VMs**: Pi-hole, AdGuard Home, Vaultwarden, Uptime Kuma, Portainer, Beszel, Home Assistant, Jellyfin, and others now default to a KVM VM (their provisioning scripts require docker/systemd, unavailable in unprivileged LXC).
- **Playwright E2E smoke suite** in `e2e/`: login + 5 routes asserted free of console errors, plus status/version checks.

### Fixed
- **Storage/ISO fixes contributed by [@kuanghl](https://github.com/kuanghl) in [#10](https://github.com/Slaker19/webkvm/pull/10)**: ISO upload limit raised from 10 GiB to 50 GiB; pools created from the UI now autostart (they went inactive after a libvirtd restart, breaking every upload); `GetISOs` refreshes the pool before listing; re-uploading a ghost file no longer fails with a permanent 409 (registered volumes and referenced files still refuse); pools without a purpose tag infer it from the name; the ISO tab uses a single pool dropdown.
- **openSUSE Leap 16.1 image URL** pointed at the unversioned "latest" build (the pinned `Build2.16` had been removed upstream and returned 404).
- **`exec` → `CommandRunner`** in the appliance deploy path (`qemu-img` resize/info), completing the testability migration already applied to `mdraid`/`zvol`.

## [0.1.10] — 2026-11-01

### Added
- **Dual View Mode in VmList**: Added toggle between Cards Grid and Dense Enterprise Table view, stored in user preferences (`localStorage`).
- **1-Click IP Copy**: Instant clipboard copy with visual feedback and toast on both cards and table without navigating to instance details.
- **Clustering Readiness**: First-class Node badge (`vm.node`) on cards and table, plus cluster node filtering chips.
- **Unified Filter Chips**: Always-visible quick filter bar with live counts for instance types (All, VMs, Containers) and states (Running, Shutoff, Paused).
- **Flexible Layer 2 & Layer 3 Network Management**:
  - Pure Layer 2 virtual switch creation without required host IP or dnsmasq (`no CIDR`), ideal for virtual routers/firewalls (pfSense, OPNsense, VyOS) and isolated zero-trust workloads.
  - Universal 802.1Q VLAN-aware trunking (`vlan_filtering 1`) supported across all Linux bridges and compatible with both QEMU/KVM guests and Incus/LXC containers.
  - First-class creation mode selector in `Networks`: Layer 2 Pure Switch, Layer 3 Host-Only, Layer 3 NAT, and Physical Direct Bridge with smart pre-population.
  - Interactive Visual Network & Port Topology Map in `Networks.svelte` displaying host uplinks, bridge structures, and connected virtual machines / containers.
  - Quick search and filtering input in `Networks.svelte` by name, CIDR, mode, and interface.
- **Background Periodic SMART Health Watcher**:
  - Native daemon `smart.Watcher` performing periodic (24h) drives sweeps, alerting on disk failure, high temperature (≥60°C), or reallocated sectors (>50) via the `notify` subsystem.
- **Terminal Session Log Downloader**:
  - 1-click `.log` export in `TerminalPanel` to download session buffers directly from serial and host consoles.

### Fixed & Hardened
- **Storage Volume Thin Provisioning**: Switched from `STORAGE_VOL_CREATE_PREALLOC_METADATA` to standard sparse creation (`0`), preventing `qemu-img` from running `preallocation=falloc` and failing with `No space left on device` (ENOSPC).
- **Select Mode Toggle Bug**: Fixed issue where the Select Mode button in `VmList` auto-exited immediately due to an eager effect when `selectedKeys` was empty.
- **Security & Vulnerabilities**:
  - Upgraded Go dependency `golang.org/x/net` to `v0.62.0`, resolving GO-2026-6866 and GO-2026-6860.
  - Pinned `minimatch` to `^11.0.3` via npm overrides, eliminating 13 moderate vulnerabilities in frontend toolchain.

### Security & Hardening
- **Multi-Tenant Batch Clone ACL Enforcement**: Enforced `assertPoolAllowed`, `assertNetworkAllowed`, and `assertPoolPurpose` in `POST /api/vms/{id}/batch-clone` to prevent restricted operators from allocating VMs into unauthorized storage pools or network bridges.
- **Storage Upload OVA Safety**: Strictly clean up temporary files and abort with HTTP 400 on `.ova` archive extraction failures in `UploadDisk`, preventing unvalidated QCOW2 files from bypassing backing-file inspections.
- **TOTP 2FA Overwrite Protection**: Mandated current password verification when re-enabling or modifying TOTP settings on accounts where 2FA is already active.
- **Proxmox Vzdump Tar-Slip Mitigation**: Enforced `filepath.Clean` and traversal component rejection in `ImportFromProxmox`, preventing malicious backup archives from overwriting container configuration metadata.
- **Recursive System Disk Protection**: Implemented recursive block device tree inspection (`checkTree`) in `assertNotSystemDisk` to safeguard complex LVM and LUKS partitioned layouts from accidental disk wiping.
- **Privileged Account Metadata Protection**: Blocked non-admin operators from setting `ci_user` to privileged system accounts (`root`, `admin`, `system`) to prevent unauthorized guest password resets.

### Changed & Refactored
- **Modular Refactoring of `VmList` Dialogs**:
  - Extracted modals into dedicated components under `lib/components/vm-list/`: `VmImportModal`, `VmImportLxcModal`, `VmAssignGroupModal`, and `VmInstantiateModal`.

## [0.1.9] — 2026-10-25

### Added
- **1-Click Incus GPU Hardware Acceleration**:
  - Support for container GPU sharing (`type: gpu`, exposing `/dev/dri/renderD128`) without detaching from host kernel, both in container creation (`VmCreate`) and hot-toggle in hardware settings (`VmHardwareTab`).
- **Incus Container PCI Passthrough**:
  - Implemented `AttachPCIDevice` and `DetachPCIDevice` in `IncusBackend`, and enabled `VmPciCard` for container instances.
- **On-Demand Backup Retention Pruning**:
  - Added `POST /api/backup/targets/{id}/prune` endpoint and CLI command `webkvm-cli backup prune <target>`.
  - Added "Prune" action button in `Backup` page for targets with retention policies configured.
- **Unified VM Scheduler**:
  - Re-routed power and automatic snapshot scheduler through neutral `compute.Backend`, ensuring cron power operations and automated snapshot retention work identically for both KVM VMs and Incus containers.
- **802.1Q VLAN Tagging in VM Detail**:
  - Added optional VLAN Tag parameter to the "Add Interface" dialog in `VmDetail` and visual VLAN badge in `VmNetCard`.

### Changed & Refactored
- **Modular Refactoring of `api/vms.go`**:
  - Decomposed 2,216-line monolith into 5 clean domain modules (`vm_lifecycle.go`, `vm_snapshots.go`, `vm_devices.go`, `vm_clones.go`, `vm_export_import.go`), reducing `vms.go` to 494 lines while retaining 100% API compatibility.

## [0.1.8-fix01] — 2026-10-18

### Security & Hardening
- **Path Injection Guard**: Enforce strict `filepath.Clean` and `safeDiskPathRE` sanitization on disk and partition target devices prior to filesystem probes in `InitHostDiskDirectory`, resolving CodeQL security alerts #125 and #126.
- **Dependency Security**: Updated `source-map-js` to 1.2.2 and `postcss-selector-parser` to 7.1.6, addressing Dependabot vulnerability alerts.

## [0.1.8] — 2026-10-17

### Added
- **Native Kernel sysfs Software RAID:**
  - Automated format, systemd persistent mount, and granular multi-folder/single-purpose pool provisioning (`CreateHostRAID`).
  - Native kernel sysfs discovery (`/sys/block/md*/md`) eliminating sub-process overhead.
  - Live array scrubbing/checking (`SyncAction`) and lifecycle control (`StopHostRAID`).
  - Non-destructive mounting of pre-existing RAID arrays without formatting (`mode: "mount"`).
- **Native Network & Pool Discovery:**
  - Unlocked discovery of native virtualization networks (`incusbr0`, `virbr0`, etc.) and default storage pools without artificial name filtering.
- **Helper Scripts Fast Ingestion:**
  - High-speed streaming catalog import via compressed tarball (625 apps in < 1 second).
  - Admin-only role lockdown and direct interactive TUI terminal invocation.

### Changed & Refactored
- **Frontend Architecture:** Decomposed `frontend/src/routes/Storage.svelte` (5,295 lines down to 542 lines) into 14 cohesive components in `src/lib/components/storage/`.
- **Backend Architecture:** Split monolithic `backend/internal/api/storage.go` (2,003 lines) into 6 domain-specific modules.
- **Safety & Resilience:** Universal `safeStorage` wrapper with in-memory fallback, memory leak cleanup in Status lifecycle, and `<firmware>` support in domain policy for UEFI backup restores.

### Fixed
- **Plain-HTTP login loop ([#5](https://github.com/Slaker19/webkvm/issues/5)):** session/CSRF cookies no longer carry the `Secure` flag when TLS is off, so browsers stop discarding the session on plain `http://` LAN deployments (dashboard flashed once and bounced back to the login page). An explicit `WEBKVM_COOKIE_SECURE` still wins when set.
- **`webkvm-cli users update`:** the subcommand advertised in the help text now exists — change password, role, email or active state from the terminal (`PUT /api/users/{username}`).
- **`webkvm-cli` local endpoint autodetection:** the CLI reads `{DATA_DIR}/config.json` (custom port, bind address, HTTP vs HTTPS) instead of guessing `https://127.0.0.1:8080`, retries once with the other scheme on an HTTPS/HTTP mismatch, and treats a machine-local bind address as local for zero-token admin access.
- **Installer no longer forces the `vmbr1` NAT bridge + dnsmasq in bridge-only mode:** isolated NAT stays opt-in (`--network=both` or `WEBKVM_NAT_BRIDGE=1`), matching the documented mode contract.

## [0.1.7] — 2026-10-11

### Added

- **Hardware & VFIO Passthrough Manager:**
  - Dedicated Hardware & VFIO management panel in Settings (`HardwareVfioTab.svelte`) with live IOMMU preflight diagnostics (Intel VT-d / AMD-Vi, DMA translation mode, and VFIO kernel module availability).
  - Visual breakdown of all system PCI devices grouped by IOMMU group, with status badges for driver, in-use instances, system bridges, and host-critical resources (boot VGA, primary uplink, storage controllers).
  - One-click driver isolation and assignment: endpoints `POST /api/host/pci-devices/{address}/bind-vfio` to bind devices to `vfio-pci` via kernel `driver_override` and `POST /api/host/pci-devices/{address}/unbind-vfio` to restore original host drivers.
  - Strict security validation against host-critical devices, path traversal, and conflicting running virtual machines.
  - Automated driver rollback: if binding to `vfio-pci` fails, sysfs `driver_override` is cleared and default drivers are reprobed to prevent leaving host devices orphaned.
- **Host Operating System Updates Manager (APT, DNF, Pacman):**
  - Integrated host OS package update manager in Settings (`HostUpdatesTab.svelte`) supporting multi-distribution package managers (APT on Debian/Ubuntu, DNF on RHEL/Rocky Linux/Fedora, Pacman on Arch/CachyOS).
  - Endpoints `GET /api/host/updates` for querying upgradable packages with automatic security advisory detection, category tabs (All, Security, Regular, Held), and pending reboot detection (`/var/run/reboot-required`).
  - Repository index refresh via `POST /api/host/updates/refresh` guarded with preflight binary checks and concurrency mutual exclusion (`isRefreshingHost`).
  - Asynchronous system upgrade endpoint `POST /api/host/updates/upgrade` executing non-interactive system updates with progress parsing, error tail extraction, and integration into the global Task Center.
- **Multi-Distribution Compatibility & Packaging:**
  - Full native runtime and installation support across Ubuntu 24.04 LTS, Rocky Linux 9.8, and CachyOS / Arch Linux.
  - Dynamic distro-aware UEFI/OVMF firmware search paths (`/usr/share/OVMF`, `/usr/share/edk2/ovmf`, `/usr/share/edk2-ovmf/x64`).
  - Standalone installer (`packaging/standalone/install.sh`) enhanced with DNF package compatibility (`iptables-nft`, optional package fallback via `pkg_available`) and co-located binary discovery.
- **Comprehensive Security Audit Hardening:**
  - **Gate A (Host Path Confinement):** Host path confinement via `internal/fsutil.ConfinePath`, restrictive file permissions (0700/0600), isolated media directories, and single-stream exclusive disk uploads.
  - **Gate B (Domain XML Policy):** Strict domain XML schema validator and single `DomainDefineXML` enforcement funnel, loopback-only VNC listeners (`127.0.0.1`), and removal of unauthenticated external VNC ports.
  - **Gate C (Untrusted Image Security):** Deep image safety validation with `imgsafe`, qemu-img probe limits, and OVA tar extraction size and entry count limits.
  - **Auth & Session Security:** Session epoch increment on password change, TOTP token replay protection, WebAuthn SignCount tracking, and hardened Content Security Policy (connect-src 'self', script nonces).
  - **Network & SSRF Defense:** IP-pinned dialer for webhooks and alerts blocking 0.0.0.0/8, 198.18.0.0/15, 240.0.0.0/4, and NAT64 ranges. Safe right-to-left X-Forwarded-For parsing.
  - **Systemd Sandboxing:** Added `ProtectHostname`, `ProtectClock`, `RestrictRealtime`, and strict ReadWritePaths to systemd service units.
- **Defensive Storage Abstraction:**
  - Added unified `safeStorage` wrapper (`$lib/utils/storage.js`) featuring transparent `MemoryStorage` fallback for restrictive browser policies and SSR contexts.

### Changed

- **Backend Modularization (`internal/libvirt`):**
  - Decoupled monolithic `domain.go` by extracting domain subsystem responsibilities into dedicated modules:
    - `domain_pci.go`: PCI address hex formatting, preflight checks, VFIO binding, and sysfs rollback.
    - `domain_usb.go`: Host USB discovery, root hub filtering, and live attach/detach.
    - `domain_shares.go`: 9p filesystem shared folders management.
- **Frontend Bundle Optimization (Vite / Rollup):**
  - Configured intelligent granular manual chunking in `vite.config.js`.
  - Entry bundle `index.js` reduced by **70%** (from 682 KB to 200 KB / 61 KB gzipped).
  - Heavy modules isolated into on-demand cached chunks (`app-i18n`, `vendor-ui`, `vendor-lucide`, `vendor-xterm`).
  - Resolved all Rollup chunk size threshold warnings.

### Fixed

- **Async Task Tracking in Host Updates:**
  - Replaced floating unhandled promise loops in `HostUpdatesTab.svelte` with synchronized `api.waitJob` execution and 35-minute operational timeouts.
- **Cloud-Init Password Generation:**
  - Fixed empty password issuance in `cloudinit.go` by ensuring cryptographic entropy with fallback PRNG and SHA-512 crypt hashing.
- **Hardware Tab Runtime Safety:**
  - Guarded preflight state access with optional chaining (`preflight?.`) to eliminate `TypeError` crashes during initialization.
- **Serial Console WebSocket Teardown:**
  - Replaced fixed sleep delays with `r.Context().Done()` channel selection in `console_serial.go` to instantly terminate backend routines upon client disconnection.

## [0.1.6] — 2026-10-04

### Added

- **Unified Background Task & Real-Time Progress System:**
  - Full integration of all asynchronous and long-running operations with the global Task Center and Task Drawer (`TaskCenter.svelte` / `TaskDrawer.svelte`).
  - Added dedicated global jobs listing endpoint `GET /api/jobs` supporting role-based access control, owner isolation, and continuous polling.
  - Automatic progress reporting across VM cloning, batch cloning, snapshots, disk/storage migrations, VM/container provisioning, appliance deployment, host disk formatting/wiping, RAID array creation, and ZFS pool lifecycle actions.
- **Host Network Bonding & Atomic Netplan Persistence:**
  - Automated Layer 2 network bond creation (`balance-rr`, `active-backup`, `802.3ad`, etc.) with atomic Netplan file generation (`/etc/netplan/60-webkvm-bonds.yaml`), syntax verification (`netplan generate`), and safe application (`netplan apply`).
  - Dedicated endpoint `DELETE /api/host/bonds/{name}` to gracefully tear down bonds and release slave interfaces.
  - Resilient uplink protection allowing secondary slave interface isolation while safeguarding primary active uplinks against accidental disconnections.
- **Native Ntfy & Gotify Alert Notification Channels:**
  - Native integration with Ntfy (`ntfy.sh` or self-hosted) with customizable server URLs, topic routing, optional Bearer authorization tokens, and priority/emoji tag mapping based on alert severity (`info`, `warning`, `critical`).
  - Native integration with Gotify self-hosted push notification server with application token authentication and numeric priority levels.
  - Interactive UI configuration cards in Settings > Notifications with live status indicator badges and masked credential storage.
  - Zero-leak HTTP transport with automatic connection teardown (`DisableKeepAlives: true`) ensuring no persistent idle loops remain after sending alerts.
- **Advanced Perimeter Defense & Security Jail (SSH, Whitelists & Custom Jails):**
  - Host SSH brute-force protection jail: live monitoring of systemd journal / auth logs to isolate SSH attackers directly at kernel level (`nftables`).
  - Whitelist CIDRs/IPs management: configure subnets and IP addresses exempt from bans with automatic unbanning upon addition and persistence in `jail.json`.
  - Manual IP banning: easily ban any malicious IP with custom duration, reason, and target origin.
  - Custom Jails: create and manage customized jail profiles with distinct failure thresholds, detection windows, and ban durations.
  - Multi-tab management interface in Settings > Security Jail with real-time statistics and i18n support (ES, EN, CA).
- **Official Grafana Dashboard (`webkvm-overview.json`):**
  - Production-ready dark overview dashboard pre-wired with host telemetry, instance metrics, and storage pools.
  - Real-time stat panels, host CPU/RAM/Disk gauges, and historical time series for compute instances (vCPU usage %, memory allocation, disk I/O read/write throughput, network transfer rates).
  - Storage pool allocation vs capacity breakdown with gradient thresholds.
  - Dedicated endpoint `GET /api/metrics/grafana-dashboard` and one-click download button in Settings > Metrics.
- **Prometheus / Alertmanager Alerting Rules (`webkvm-alerts.yml`):**
  - Standard Alertmanager alerting rule definitions covering daemon reachability (`WebKVMDown`), host CPU/RAM/disk saturation, storage pool thresholds (>85%), and sustained VM CPU spikes.
  - Dedicated endpoint `GET /api/metrics/alert-rules` and one-click download/copy buttons in Settings > Metrics.
- **Full Internationalization (i18n):**
  - Complete multilingual translations across English, Spanish (`es`), and Catalan (`ca`) for Ntfy, Gotify, Grafana dashboard, and Alertmanager rules.

## [0.1.5] — 2026-10-03

### Added

- **Native Prometheus / OpenMetrics Exporter (`/metrics` & `/api/metrics/prometheus`):**
  - Standard Prometheus text format scraping endpoints exposing complete operational telemetry.
  - Process and build metadata (`webkvm_info`, `webkvm_up`).
  - System host metrics (CPU usage %, RAM total/used/free bytes, disk capacity and utilization, network rx/tx bandwidth, and system uptime).
  - VM state aggregates and per-instance live telemetry (`webkvm_vms_total`, `webkvm_vms_state`, `webkvm_vm_status`, `webkvm_vm_vcpus`, `webkvm_vm_cpu_usage_percent`, `webkvm_vm_memory_allocated_bytes`, `webkvm_vm_memory_used_bytes`, `webkvm_vm_disk_size_bytes`, `webkvm_vm_uptime_seconds`, and live disk I/O and network transfer rates).
  - Storage pool metrics (capacity, allocation, availability, and active status per pool).
- **Flexible Exporter Authentication & Security:**
  - Support for standard `Authorization: Bearer <token>` and `?token=` query parameters (accepting API tokens and session credentials).
  - Hot-reloadable setting `metrics.allow_unauthenticated` enabling seamless scraping on trusted private LANs.
  - Hot-reloadable setting `metrics.prometheus_enabled` allowing operators to toggle the scraping endpoint on or off.
- **Prometheus Scraper UI & Integration Guide:**
  - Dedicated Prometheus integration card in Settings under the new Metrics section.
  - One-click copy for the full `/metrics` endpoint URL and complete ready-to-use `prometheus.yml` scrape configuration job snippet.
  - Complete internationalization across English, Spanish, and Catalan.

## [0.1.4] — 2026-10-03

### Added

- **Multi-Stage Container Build & Pre-Flight Diagnostics:**
  - Multi-stage `Dockerfile` with isolated Svelte frontend compilation (Node 22 slim) and CGO/libvirt backend build (Ubuntu 24.04 with Go 1.26), ensuring full binary and glibc compatibility regardless of host environment.
  - Layer caching optimization for fast rebuilds (~15 seconds).
  - Pre-flight diagnostic checks in `docker-entrypoint.sh` verifying libvirt socket connectivity, hardware virtualization acceleration (`/dev/kvm`), and ZFS kernel module availability (`/dev/zfs`).
  - Container health check integration (`HEALTHCHECK`) and host parity configuration (`pid: host`, systemd units, `/dev:rslave`).
- **Live Memory Ballooning:**
  - Dynamic memory adjustment on running virtual machines (`SetMemoryFlags` with `DOMAIN_MEM_LIVE`) without requiring VM shutdown or reboot.
  - User interface indicator in VM settings alerting when live memory ballooning is active on running instances.
- **Proactive Storage Degradation Alerts:**
  - Expanded `AlertEngine` monitoring physical drive S.M.A.R.T. health, uncorrectable pending sectors, and NVMe critical hardware warnings (`CriticalWarning`).
  - Automatic critical alerts for degraded, faulted, or unavailable ZFS storage pools (`zpool`).
  - Automatic detection of degraded software RAID arrays (`mdadm`) via `/proc/mdstat`.
  - Refined ZFS pool status indicators with color-coded severity (`ONLINE`, `DEGRADED`, `FAULTED`/`UNAVAIL`).
- **Comprehensive Internationalization (i18n):**
  - Full multilingual support (English, Español, Català) for newly added components: S.M.A.R.T. Telemetry modal (`SmartModal`), NetGuard Security Jail tab (`SecurityJailTab`), and Batch VM Cloning modal (`BatchCloneModal`).

### Fixed

- **ZFS ZVol Device Resolution in Containers:**
  - Added adaptive retry and synchronization loop with `udevadm settle` in `zvol.Resolve()` to reliably locate `/dev/zvol` and block device nodes created asynchronously in containerized environments.
- **VM Boot Device Compatibility:**
  - Accepted `"hd"` and `"disk"` as valid boot device identifiers, mapping seamlessly to the libvirt XML domain schema.
- **Static Analysis & Slice Allocation (CodeQL):**
  - Eliminated high-severity taint tracking alert in batch clone handler by dynamically sizing result slice with `var cloned []models.VM`.

## [0.1.3] — 2026-10-01

### Added

- **SPICE Console & Streaming Proxy:**
  - Full Web SPICE console client (`spice.mjs`) with dynamic auto-scaling viewport fit, aspect ratio preservation, and vdagent monitor resizing (`VD_AGENT_MONITORS_CONFIG`).
  - Scancode Set 1 hardware keyboard mapping for physical and virtual keyboards (ES ISO and US ANSI layouts) with `AltGr` synthetic Ctrl suppression and extended key coverage (`º`, `\`, `ç`, `ñ`, `< >`, accents).
  - Sub-pixel mouse coordinate scaling via `getBoundingClientRect()`.
  - High-performance streaming WebSocket proxy for SPICE graphics with ping/pong keepalives and atomic goroutine termination.
- **S.M.A.R.T. Health Telemetry:**
  - Concurrent physical disk inspection and S.M.A.R.T. metrics retrieval (`smartctl`), displaying disk health, temperature, power-on hours, bad sectors, and error logs in a dedicated dashboard modal.
- **NetGuard Jail & Brute-force Protection:**
  - Hardened IP blacklist isolation and rate-limiting jail preventing unauthorized access, with real-time UI inspection (`SecurityJailTab.svelte`).
- **Batch VM Cloning:**
  - Batch clone modal and API enabling parallel duplication of virtual machines with custom naming prefixes and sequential numbering.

### Fixed

- **Cloud-Init VM Deployment on Storage Pools & Graphics Compatibility:**
  - Resolved 400 Bad Request error (`purpose "disk", expected "container"`) and silent failure when deploying Cloud-Init VMs on libvirt pools such as `webkvm-disks` by explicitly passing `type: "vm"` in the wizard payload.
  - Added support for `"completed"` terminal job status in client-side job polling (`waitJob`), preventing infinite loading spinners upon instance provisioning.
  - Safe automatic degradation to VNC graphics (`resolveGraphicsType`) on hosts whose QEMU binary (such as QEMU 10+) lacks SPICE server support.
  - Fixed autogenerated deployment password length in App Store to strictly conform with Cloud-Init requirements (6 to 12 characters).
- **Language Selector in Compact & Hover Sidebar:**
  - Fixed dropdown menu flickering, jumping, and unexpected auto-closing when toggled in compact (rail) and auto-expand (hover) sidebar modes.
  - Sidebar maintains its expanded state while floating menus are open and prevents focus-loss collapses.
  - In compact rail mode, the language menu opens to the right (`side="right"`, `align="end"`) avoiding off-screen left clipping.
  - Removed conflicting anchor-width constraints from DropdownMenu primitives in Tailwind v4.

## [0.1.2-fix2] — 2026-09-30

### Fixed

- **Storage Pool Deduplication & Capacity Calculation:**
  - Added missing `DeviceID` (`st_dev`) resolution to Incus storage pools in `IncusBackend.ListStoragePools()` and `CreateStoragePool()`.
  - Prevented multi-accounting and duplicate summing of disk capacity and used allocation across shared host filesystems between KVM and Incus storage pools.

## [0.1.2-fix1] — 2026-09-30

### Security

- **Path Injection Hardening:**
  - Strengthened path traversal defenses in `zvol.Resolve` and `mdraid.ensureDeviceNode` using `filepath.Clean`, explicit prefix boundaries, and localized regex checks.
- **Dependency Vulnerability Fixes:**
  - Resolved Dependabot alerts (CVE-2026-102277 / GHSA-q2hr-2g5m-vwhr) by locking `brace-expansion` dependencies to patched versions (`1.1.21` and `5.0.12`) via npm overrides.

## [0.1.2] — 2026-09-30

### Added

- **ZFS Storage Pools & ZVols:**
  - Management of ZFS storage pools with support for stripe, mirror, raidz1, and raidz2 topologies.
  - Creation and lifecycle of raw block devices (ZVols) with thin provisioning (sparse) support for direct native VM attachment.
  - Dedicated REST API endpoints: `GET /api/host/zpools`, `POST /api/host/zpools`, `GET /api/host/zvols`, `POST /api/host/zvols`, and `DELETE /api/host/zvols/{name}`.
  - Dedicated ZFS management tab in the WebKVM Storage dashboard with pool and volume creation wizards.
- **Linux Software RAID (mdadm):**
  - Creation and management of host software RAID arrays supporting RAID levels 0, 1, 5, 6, and 10.
  - Automatic persistence to `/etc/mdadm/mdadm.conf` and safe disk/mount validation checks.
  - Dedicated REST API endpoint: `POST /api/host/raid`.
  - Interactive RAID array creation wizard in the Storage view.
- **Host Disk Filtering:**
  - Automatic exclusion of squashfs, iso9660, and snap loop devices from host physical disk listings.
  - Recognition and support for `/dev/md*` software RAID block devices.
- **Live VM hardware & ballooning:**
  - Memory ballooning floor (`min_ram_mb`) support for dynamic memory reclaiming with guaranteed lower bounds.
  - Dedicated IOThreads (`iothreads`) support for VirtIO-SCSI storage controllers and event loop concurrency.
- **Cloud-Init & Direct Network Configuration:**
  - Direct Linux bridge network configuration with custom static IP/Subnet (CIDR), default gateway, and primary/secondary DNS server support.
  - Static IPv4/IPv6 CIDR, default gateway, and DNS configuration in Cloud-Init NoCloud seeds.
  - Visual selector in creation and edit wizards between DHCP and Static IP modes.
- **QEMU Guest Agent extensions:**
  - Filesystem TRIM (`fstrim`) API endpoint `POST /api/vms/{id}/guest/fstrim` and UI button in Guest tab.
  - Active guest sessions/users (`guest-get-users`) and guest timezone (`guest-get-timezone`) telemetry.

### Fixed

- **Netplan & NetworkManager Compatibility in Setup Scripts:**
  - Prioritized Netplan as the top-level declaration layer on modern Ubuntu/Debian systems, avoiding conflicting ephemeral network units.
  - Fixed NetworkManager bridge creation by referencing connection UUIDs to prevent word-splitting failures on connection names with spaces (`Wired connection 1`).
  - Added proper bridge slave profile generation (`nmcli con add type ethernet master ...`) in NetworkManager mode to ensure carrier persistence across reboots.
  - Filtered out IPv6 nameservers from IPv4 static DNS detection to prevent invalid NetworkManager keyfile generation and startup rejections.
  - Disabled `systemd-networkd` when NetworkManager is the active Netplan renderer, preventing dual-manager boot deadlocks in `*-wait-online.service`.
  - Added async interface polling loops to bridge setup steps to reliably await sysfs bridge creation.

## [0.1.1] — 2026-09-29

### Added

- **In-app self-update:**
  - Automated update support via `POST /api/system/update` and `/#/status`.
  - Automatic detection of update mode: **release** (downloads and installs verified GitHub release binaries) or **source** (pulls git checkout and rebuilds frontend/backend).
  - Checksum verification against published `SHA256SUMS` with fail-closed security before running root installers.
  - Transient systemd unit execution via `systemd-run` to isolate the updater from the main service cgroup during service stops and restarts.
  - Concurrency locking using `flock` on `/run/webkvm-update.lock`.
  - Staging and atomic binary replacement with automatic health check and rollback recovery.
  - Dynamic service port detection from `config.json` and systemd environment.
  - Status UI buttons, modals, and translations in English, Spanish, and Catalan.

## [0.1.0] — 2026-09-29

### Added

- **Incremental backup chains:**
  - Libvirt push-mode incremental backups with `CheckpointEntry` disk tracking.
  - Multi-disk checkpoint support with `DiskFiles` mapping to prevent cross-device rebase issues.
  - Target-staged restore to avoid `EXDEV` errors across mounted filesystems.
  - Chain-aware retention policy protecting incremental chains during pruning.
  - External storage pool support for `.qcow2` listings across SFTP, S3, and local directories.

### Fixed

- Sanitized storage pool and target destination paths to resolve security alerts.
- Multiline domain XML disk parsing and CD-ROM exclusion during backups.

---

## [0.0.1] — 2026-09-26

First public release of WebKVM: a self-hosted web panel (a single Go
binary with the Svelte frontend embedded) for managing a Linux
virtualisation host.

### Added

- **KVM/QEMU virtual machines via libvirt:** creation, cloning, hardware
  editing, in-browser VNC and serial console, snapshots, disks and
  storage migration between pools.
- **Incus/LXC containers** (optional module) managed from the same
  interface as VMs, including the Incus image catalogue. Containers pick
  their storage pool at creation time and can be moved between
  `container` pools afterwards — rootfs and snapshots relocated by
  Incus's native migration, with the instance stopped.
- **Storage:** local and network pools (NFS/SMB) with purposes
  (`disk`, `container`, `iso`, `backup`, `template`), physical disk
  inspection and a single media store for ISOs and images.
- **Networking:** bridges, NAT/isolated networks and unified
  Proxmox-style L2 networks, with a per-VM firewall.
- **Backups**, scheduled and on demand, with restore plus
  import/export in Proxmox vzdump format for containers.
- **Templates** for VMs and containers, with an admin-only
  "share with all users" toggle per template: a shared template is
  listable and instantiable by everyone regardless of owner, group or
  tag access, and every instantiation copies its full disk.
- **Users, roles and quotas:** RBAC with per-resource ACLs, resource
  quotas, 2FA/TOTP, API tokens and an audit log.
- **Cloud-init** for initial guest configuration (users, SSH keys,
  network, packages).
- **App store:** ready-to-deploy appliances and helper scripts, plus a
  catalogue of official cloud images.
- **PCI and USB passthrough** with an IOMMU/VFIO preflight check.
- **Interface in three languages:** English, Spanish and Catalan.
- **Installation:** multi-distro standalone installer (apt/dnf/pacman)
  with self-signed HTTPS and rollback, a Docker image, deb/rpm packages
  and a command-line client (`webkvm-cli`).

---

[0.1.11]: https://github.com/Slaker19/webkvm/releases/tag/v0.1.11
[0.1.10]: https://github.com/Slaker19/webkvm/releases/tag/v0.1.10
[0.1.4]: https://github.com/Slaker19/webkvm/releases/tag/v0.1.4
[0.1.3]: https://github.com/Slaker19/webkvm/releases/tag/v0.1.3
[0.1.2-fix2]: https://github.com/Slaker19/webkvm/releases/tag/v0.1.2-fix2
[0.1.2-fix1]: https://github.com/Slaker19/webkvm/releases/tag/v0.1.2-fix1
[0.1.2]: https://github.com/Slaker19/webkvm/releases/tag/v0.1.2
[0.1.1]: https://github.com/Slaker19/webkvm/releases/tag/v0.1.1
[0.1.0]: https://github.com/Slaker19/webkvm/releases/tag/v0.1.0
[0.0.1]: https://github.com/Slaker19/webkvm/releases/tag/v0.0.1
