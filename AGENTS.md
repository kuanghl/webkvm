# WebKVM — Contexto para desarrollo (AGENTS)

> Documento vivo para agentes y humanos. Actualizar al cerrar cada fase.
> Última actualización: 2026-11-16. Rama `feature/ssh-machines` (sobre `main` `def9cb3`); el trabajo F15/F15.1/F15.2/F15.3/F15.4/F15.5 está en el working tree, sin commitear.

## 1. Qué es

WebKVM convierte cualquier Linux (Ubuntu/Debian/Fedora/Arch/Alma/Rocky) en nube privada en 1 min, sin reinstalar.
Binario único Go ~14 MB, ~15 MB RAM en idle. Gestiona **QEMU/KVM + Incus/LXC** bajo un solo dashboard.

- Web UI: Svelte 5 (runes) + Tailwind v4 + xterm WebGL + noVNC, servida **embebida** en el binario.
- Server: Go 1.26 + CGO libvirt + chi + JWT + WebSocket + SSE.
- CLI: `webkvm-cli` (Go puro, sin CGO), zero-token como root.
- Instalación: `scripts/install-webkvm.sh` → `packaging/standalone/install.sh` (nativo systemd) o Docker (`docker-compose.yml`).
- Docs usuario en `docs/` (cada `.md` tiene espejo `.es.md`). No duplicar aquí guías de uso; esto es contexto técnico.

## 2. Mapa del repo

```
webkvm/
  backend/           Go (module webkvm, go 1.26.9). 509 .go, ~142k líneas (94k sin tests), 267 *_test.go
    cmd/server/      main.go (1174L) + tls.go — único binario prod
    cmd/cli/         main.go (2205L) — webkvm-cli
    cmd/migrate-disk-names/  one-shot renombrado discos
    internal/        41 paquetes + subpaquetes compute/incus y sshmachines/zmodem (ver §3.2)
    internal/frontend/dist/  copia de ../frontend/dist (la hace make, no editar a mano)
  frontend/          Svelte 5 + Vite 6. 316 ficheros en src/ (213 .svelte, 41 *.test.js)
    src/routes/      25 componentes de ruta hash `#/...` (VmList, VmDetail, VmCreate, MachinesList, MachineDetail, MachineCreate, ...)
    src/lib/components/vm-detail/  16 submódulos (overview/hardware/disks/net/usb/pci/console/metrics/...)
    src/lib/components/machine/    SftpBrowser (wrapper 15L), HostKeyDialog, ZmodemPanel, zmodemClient + files/ (workspace: FilesWorkspace, FilePane, FileList, TransferQueue, ConflictDialog, EntryDialogs, MachinePicker, PathBreadcrumb, PaneToolbar, paneModel, fileActions)
    src/lib/stores/  16 stores .svelte.js (auth, events, files, tasks, theme, terminalSettings, ...)
    src/lib/utils/   ~30 helpers, casi todos con su *.test.js
    src/lib/i18n.svelte.js  12.3k líneas, locales en/es/ca
    dist/            salida vite (no editar; la regenera make)
  scripts/           instaladores y utilidades (ver §5)
  packaging/         standalone/ (install/update/uninstall canónicos), nfpm deb/rpm, arch PKGBUILD, docker/
  docs/ + docs/runbooks/  guías usuario y runbooks ops
  e2e/               suite Playwright (login + rutas sin errores de consola)
  Dockerfile, docker-compose.yml, docker-entrypoint.sh
  Makefile (SHELL=bash, VERSION sin `v`), install.sh, .env.example
  .github/workflows/  ci.yml, release.yml, docker-publish.yml
```

## 3. Backend (Go)

### 3.1 Entradas
- `backend/cmd/server/main.go`: carga `config.Load` + `configstore`, TLS (`tls.go`: off/selfsigned/letsencrypt), abre libvirt, `EnsureDefaults + CleanupStaleImports`, inyecta todo en `api.NewRouter(...)` (sin globales salvo `libvirt.SetNetStore/SetNATChecker`, `compute.BindHelpers`). HTTP: 30s header / 30m write / 120s idle + `LimitListener` + `Cooldown`. Shutdown ordenado con `FlushAndWait()` y `hub.Close()` antes.
- `backend/cmd/cli/main.go`: `--server/--token/--insecure/--json`, `login/logout`, zero-token local firmando HS256 con `/opt/webkvm/jwt.key` como admin. Subcomandos: `vms/images/storage/disks/networks/settings/audit/users/tokens/backup/snippets`.
- `backend/internal/frontend/embed.go`: `//go:embed all:dist`. El frontend se sirve desde aquí.

### 3.2 Paquetes `internal/` (41 + subpaquetes)
`api appliances audit auth backupstore cloudinit compute(/incus) config configstore diskprobe diskstate events firewall frontend fsutil helperscripts hostcaps imgsafe libvirt libvirtbackup logging mdraid metrics models monitor netguard netstore nodes notify remotebrowse safego serialx smart sshmachines(/zmodem) sysutil telnet tokens user vmsched vzdump zvol`

Ficheros grandes actuales (orientación; descomponer antes de editar a fondo):
`backupstore/runner.go:2750 cmd/cli/main.go:2205 api/host_disks.go:1834 api/console.go:1427 monitor/host_monitor.go:1757 backupstore/store.go:1621 models/types.go:1529 libvirt/domain.go:1425 libvirt/storage.go:1399 compute/incus/incus_images.go:1312`
Los monolitos históricos **ya están descompuestos**: `api/vms.go` (494L + 5 `vm_*.go`), `api/storage*.go` (7 submódulos), `libvirt/domain*.go`, `compute/incus/*` (5 submódulos), `routes/Storage.svelte` (542L + 14 comps).

### 3.3 Módulos clave
- **Libvirt/KVM** `internal/libvirt/`: `domain.go` + `domain_{xml,lifecycle,devices,snapshots,pci,usb,shares,policy,export_import}.go`, `storage.go`, `network.go`, `guestagent.go`, `ova.go`, `secret.go`, `metrics.go`, etc.
- **Compute seam** `internal/compute/`: `backend.go` interfaz `Backend` (~80 métodos). `kvm.go` adaptador delgado, `combined.go` KVM+Incus, `incus/` en 5 submódulos. Handlers **nunca** importan libvirt directo: van vía `compute.Backend`.
- **Auth** `internal/auth/`: `jwt.go` (Manager, Claims epoch/jti/mcp, CSRF double-submit), `rbac.go`, `ticket.go` (ticket `?ticket=` 30s un solo uso para serial/host-PTY/SSE), `vnc_ticket.go` (`?vt=` 1h reutilizable acotado a VM), `cookie/ratelimit/blacklist/totp/webauthn/must_change.go`.
- **API** `internal/api/`: `router.go` (chi + Recoverer/securityHeaders/requestLogger/limitBody/cors/JWT/rateLimit/SessionEnforcer), `handler.go` (todo inyectado + `deployLock` por nombre VM), `vm_*.go`, `storage_*.go`, `console.go` + `console_serial.go` (`creack/pty`, single-flight por VM, timeout inactividad), `events/networks/firewall/cloudinit/backup/host_*.go`, `middleware_*.go`, `authz.go`, `capability_gate`.
- **Máquinas remotas** `internal/sshmachines/`: `store.go` (registro `ssh-machines.json` + secretos en fichero 0600 aparte; la API solo expone `has_password`/`has_key`), `dial.go` (ssh | telnet vía `internal/telnet` | serie vía `internal/serialx`), `sftp_fs.go`+`sftp_session.go` (seam `sftpFS`/`sessionSource` + **leases**, `Acquire`/release idempotente; los tests usan un `sftpFS` en memoria, nunca un cliente real), `transfers.go`+`transfers_dir.go`+`dirwalk.go`+`putfile.go` (jobs de archivo/directorio/copia máquina↔máquina en **streaming directo**; `dirwalk` con pila propia — **no** `Client.Walk` — topes 32 niveles/20k entradas, symlinks jamás seguidos), `zmodem/` (receptor `core`+`frame`+`parser`+`state`+`session` y emisor `sender`) + `zmodem_job.go` + `zmodem_upload.go` (staging con TTL), `local_sftp.go` (tarjeta `host` in-process), `host.go` (`UpdateHost`/`HostMachine`: la tarjeta `host` persiste su cuenta y su contraseña — secretos bajo la clave `host` del mismo fichero 0600; es la única `builtin:true` que se guarda). Handlers: `api/sshmachines{,_sftp,_terminal,_zmodem,_shutdown}.go` + `api/machine_transfers.go` + `api/host_credentials.go` (tarjeta `host`: `PUT`/`test` solo admin + probe de login real por PTY + auto-feed del terminal). Reglas: capability `console` para **todo el plano de lectura** (`GET /api/ssh-machines` y `/{id}` incluidos) + terminal + SFTP + transferencias; `operator+` para el CRUD (`hostkey` también); `rejectBuiltin` en las superficies que **marcan o borran** (terminal/ticket/`hostkey`/shutdown/DELETE); la tarjeta `host` **sí** es navegable en el plano de ficheros (par `net.Pipe()` con `sftp.NewServer(..., WithServerWorkingDirectory("/"))` + `sftp.NewClientPipe` por la misma registry de leases) pero es **solo admin** (`gateHostCard` responde 404 a un no-admin, igual que un id inexistente, porque lee y escribe con los privilegios de webkvm); lo escribible de la tarjeta (`PUT /host`, `POST /host/test`) también es solo admin con el mismo 404, y el auto-login del terminal es de **una sola vez** (user+password como máximo una vez cada uno; rechazo, re-prompt o 30 s devuelven el prompt intacto al operador). Propiedad de job solo vía `machineJob`, progreso solo por el SSE `transfer.progress`, y toda escritura remota por `putFile`/`putStream` (`.webkvm-part-<job>` + rename atómico). SSH exige pin de host key la primera vez (409 → `HostKeyDialog.svelte`). En el WS del terminal un frame de control **es su `type`** (`resize`|`zmodem`); lo demás, incluidos los bytes que empiezan por `{`, es entrada cruda. Los buffers de lectura del remoto (telnet `pending`, serial `overflow`, hold-back IAC) están acotados.
- **SSE** `internal/events/hub.go` (buffer 16, drop slow clients, **nunca re-emite** lo que el socket se perdió). Origen: `libvirt.StartEventLoop` + `MetricsCollector.SetSink`. Frontend: `events.svelte.js` con backoff 1s→30s; el handler `error` ignora los fantasmas de fuentes sustituidas/cerradas (`this._es !== es`) y el `open` limpia el timer de reconnect pendiente → tras un tirón no hay bucle de reconexión (regresión cubierta en `eventsReconnect.dom.test.js`). El hook `events.onOpen(fn)` es **el punto de resync** de todo estado derivado de eventos (badge running del Sidebar, VmList, VmDetail).
- **Storage/Red**: vía `compute.Backend` → libvirt; más `zvol/`, `diskprobe/`, `diskstate/`, `netstore/`, `remotebrowse/` (SMB/NFS).
- **Transversales**: `backupstore/` (store/runner/producer/retention/s3/sftp/chain), `metrics/` (history/grafana/prometheus/alerts), `monitor/` (host_monitor + watcher SMART), `notify/`, `nodes/registry.go`, `audit/audit.go`, `config/` + `configstore/` (hot-reload), `netguard/` (jail/limitlistener/cooldown/ssh_watcher), `vmsched/` (cron), `vzdump/`, `appliances/` (App Store, ingesta streaming por tarball), `models/types.go`.
- **WS/PTY** VNC/SPICE: `gorilla/websocket` en `console.go` (binario, ping 25s).

### 3.4 Patrones obligatorios
1. **`sysutil.CommandRunner`** (`sysutil/runner.go`): `OSCommandRunner` en prod, `MockCommandRunner` en tests. Todo `exec` nuevo **debe** usarlo (inyectado). Ref: `smart/smart.go`, `mdraid/`, `zvol/`, `hostcaps/`.
2. **`context`**: `exec.CommandContext`, `RunWithTimeout`, timeout 15s en virsh guest-agent. Nada de `time.Sleep` rígido en backend: usar `<-ctx.Done()` / canales.
3. **Tickets, no JWT en URL**: JWT solo en cookie HttpOnly o `Authorization: Bearer`. En URLs solo `?ticket=` (30s/1 uso) o `?vt=` (consola). Validar scope por path (`ticketAllowedPath` / `vncTicketAllowedPath`).
4. **Seam `compute.Backend`**: handlers contra interfaz, no contra libvirt.
5. **Sin `panic()` en prod**: devolver error + defaults seguros.
6. **Seguridad storage/imágenes**: `backing_guard`, `imgsafe`, `confine/path_safety`, pools fail-closed, dialer anti-SSRF (bloquea 10/8, 172.16/12, 192.168/16, 127/8, 169.254/16, 100.64/10, fc00::/7...), firewall con ventana de confirmación 30s + rollback.
7. **Tests**: 267 `*_test.go`. Convención: unit con mocks (`CommandRunner`, stores temp), sin integración real. Cobertura fuerte en auth/RBAC, XML libvirt, storage/backup, red/firewall, métricas.

## 4. Frontend (Svelte 5)

- Router hash en `src/lib/router.svelte.js`. Rutas: `vms`, `vms/new`, `vms/:id`, `machines`, `machines/new`, `machines/host` (estática, admin), `machines/:id`, `apps/images/multiview/storage/media/networks/backup/firewall/audit/users/nodes/snapshots/system/settings/account`, `login`. No hay `#/files` ni `#/host-console`: el explorador de ficheros vive en la pestaña Files de `MachineDetail`, y la terminal del host es la máquina `host`.
- `src/App.svelte`: lazy `routeLoaders`, ciclo SSE (`events.svelte.js`), RBAC, `HostConsole` persistente.
- `routes/VmDetail.svelte` (~3100L, orquestador con tabs) + `lib/components/vm-detail/` (16 piezas). No re-monolitizar.
- `TerminalPanel.svelte` (~950L): xterm + Fit/Webgl(fallback DOM)/Unicode11/WebLinks/Clipboard. `mode=vm` → `/api/vms/{id}/serial?ticket=`, `mode=host` → `/api/host/terminal?ticket=`, `mode=machine` → `/api/ssh-machines/{id}/terminal?ticket=` con `machineProtocol` como prop. Los frames de texto del WS son JSON (`{"cols","rows"}` de resize; `{"type":"zmodem",…}` para armar/cancelar la recepción o, con `{"dir":"up","upload_id":…}`, una subida desde el stage del host); los binarios son tty crudo: el sniff nunca se come un byte salvo en el marco explícito de `machine/zmodemClient.svelte.js`, que además reintroduce en el terminal las teclas escritas mientras esperaba al remoto. El panel `{Dialog+strip}` es `machine/ZmodemPanel.svelte` (solo se renderiza en telnet/serial). Temas en `utils/terminalConfig.js`, prefs en `stores/terminalSettings.svelte.js` (`webkvm.term.*`).
- El workspace de ficheros es `components/machine/files/FilesWorkspace.svelte`, montado **solo** como pestaña Files de `MachineDetail` (`SftpBrowser.svelte` es su wrapper de 15 líneas). Una columna por defecto con la máquina de la página fijada; la segunda columna aparece al pulsar el botón de transferencia (`openTransfer`/`closeTransfer` como **props**, nunca dentro del bag `actions`, que congela qué botón se renderiza). Estado compartido en `stores/files.svelte.js` (Map de jobs + `ensureSSE()` idempotente, el estado terminal de un job **no puede retroceder**, `_toSave` entrega la descarga al navegador cuando el job termina) y puros en `utils/fileTransfer.js` (`carriesFiles`, `planUploads`, `autoRename` acumulativo, `flattenDrop` recursivo). Preferencias `webkvm.files.*` con `safeStorage`.
- `routes/MultiView.svelte`: video-wall noVNC vía `iframe /console/{id}?vt=&embedded=1`, grid 1-4 cols, `postMessage` broadcast, persistencia `webkvm.multiview.*`.
- `stores/tasks.svelte.js` + `TaskDrawer.svelte` + `TaskCenter.svelte`: drawer `Ctrl+J`, altura 160-600px (`webkvm.taskdrawer.height.v1`), split lista+logs.
- `stores/events.svelte.js`: `EventSource /api/events` (cookie, sin ticket), backoff 1s→30s. Un solo punto de consolidación SSE: no crear más `setInterval` de polling. Quien derive estado de eventos (p. ej. el `runningCount` del Sidebar, la lista de VmList, VmDetail) **debe** registrar `events.onOpen(resync)`: el hub nunca re-emite lo perdido y un `vm.state` perdido deja el estado caduco para siempre.
- i18n `src/lib/i18n.svelte.js`: `t(key,vars)`, locales `en/es/ca`, persistencia `webkvm.locale`. **Toda string visible debe tener clave en los 3 idiomas.** Verificar con `scripts/check-i18n.sh`.
- Componentes capitalizados deben estar importados (verifica `scripts/check-undefined-components.sh`).
- Build: `vite build` → `frontend/dist/` (`manualChunks`: vendor-xterm/novnc/lucide/ui/cron/qr, app, i18n). `make build-backend` copia a `backend/internal/frontend/dist/` y compila con CGO. Dev sin embed: `make dev-backend` (go run :8080) + `make dev-frontend` (vite :5173 con proxy a 8080).
- Tests: `npm test` (27 node puros `src/lib/**/*.test.js`) y `npm run test:dom` (14 happy-dom `*.dom.test.js`). Config en `vite.config.js` / `vitest.dom.config.js`.

## 5. Ops / build / install

- `make build` = frontend + backend (`webkvm`) + cli (`webkvm-cli`). `make binary/dist` empaqueta tarball + 2 binarios + `SHA256SUMS`. `make deb/rpm` (nfpm), `make docker-build`.
- `make install-systemd`: instala en `/usr/local/bin`, guarda `.previous`, health-check 20s con auto-`rollback`. `make rollback` restaura. `make install` (completo), `install-systemd-force` (sin check), `install-caddy` (`SKIP_INSTALL=1` solo Caddyfile), `regen-cert`, `install-all`, `uninstall` (conserva `/opt/webkvm` y logs).
- Instalador canónico `packaging/standalone/install.sh`: detecta apt/dnf/pacman, instala runtime (libvirt/qemu/ovmf/swtpm), despliega binario precompilado (nunca compila en destino), escribe `config.json`, TLS, `enable --now`, health-check. Vars: `WEBKVM_*`, `NETWORK_MODE`, `NONINTERACTIVE`.
- `scripts/`: `setup-network.sh` (bridge/NAT), `setup-bridge.sh`, `generate-self-signed.sh`, `install-caddy-systemd.sh`, `detect-webkvm-endpoint.sh` (usado por Makefile, no hardcodear 8080), `smoke.sh` (E2E pool→volumen→define→start→teardown), `build-in-docker.sh`, `webkvm-backup.sh`, `check-i18n.*`, `check-undefined-components.*`.
- Systemd `scripts/webkvm.service`: `After libvirtd/network-online`, root, `DATA_DIR=/opt/webkvm`, `ExecStart=/usr/local/bin/webkvm`, hardening con `ReadWritePaths`.
- Docker: imagen `slaker1908/webkvm:latest`, `network_mode:host`, `privileged:true`, `pid:host`. Monta `/opt/webkvm` en misma ruta, `/dev:rslave`, `/var/run/libvirt`, sockets systemd, `/etc/passwd+shadow+group+pam.d:ro`. No bundlea libvirtd.
- CI `.github/workflows/`: `ci.yml` (frontend: lint+prettier+i18n+undefined+test+build; backend: libvirt-dev + reuse artifact frontend-dist + golangci-lint + `go test -race`; packaging: `test-installer.sh`), `release.yml` (tag `v*` → tarball+deb/rpm+SHA256 fail-closed), `docker-publish.yml`.
- Datadir `/opt/webkvm` es única fuente de estado (users, jwt.key, config, pools-meta). Backup sin qcow2/iso.

## 6. Estado actual

Detalle completo por fase en `CHANGELOG.md` (`[Unreleased]` = F15/F15.1/F15.2/F15.3/F15.4/F15.5). F1–F15.5 cerradas; F15/F15.1/F15.2/F15.3/F15.4/F15.5 **sin commitear** en `feature/ssh-machines`.

- **F1–F6 (base)**: bind `0.0.0.0:8080` + `build-in-docker.sh`; `VmDetail` 6479L→16 comps, i18n 100%; `domain.go` 4048→~1400L; sleeps→`ctx.Done()` + SSE consolidado; `CommandRunner` OS/Mock y sin `panic`; validado en systemd + `smoke.sh`.
- **F7 (0.1.8-dev)**: RAID nativo por sysfs (`/sys/block/md*`), pools en arrays con formateo directo o `mode:"mount"` no destructivo, scrub/check en vivo + tarjetas UI.
- **F8 (0.1.8-dev)**: refactor integral de storage (`Storage.svelte` 5295→542L en 14 comps; `api/storage.go` 2003L en 7 submódulos), `safeStorage` universal, blindajes path-traversal/UEFI/409-suspensión, E2E 54 pruebas OK.
- **F9 (0.1.8-dev)**: App Store por ingesta streaming en 1 tarball (625 scripts en 0.65s, sin rate-limit GitHub), credenciales auto, tarjetas con puerto/tags.
- **F10 (0.1.8)**: issue #5 — cookies `Secure` con TLS off, `webkvm-cli users update`, autodetección de endpoint local, instalador sin `vmbr1`+dnsmasq en solo-bridge.
- **F11 (0.1.9)**: GPU 1-clic + passthrough PCI en Incus; `api/vms.go` 2216→494L en 5 submódulos; poda de backups bajo demanda; VLAN 802.1Q.
- **F12 (0.1.10)**: switches L2 puros + trunking VLAN unificado KVM/Incus, mapa de topología, watcher SMART, export terminal `.log`, remediations de auditoría Cloudflare, acciones en bloque, redes en CLI.
- **F13 (0.1.10-dev)**: aceleración 3D virgl/egl-headless por VM (cubierto el bug de `<graphics>` borrado); rescate de arreglos del PR #10; Revert de snapshots por `has_memory`.
- **F14 (0.1.10-dev)**: actualizador con tag+commit, noVNC embebida en `VmDetail`, apps Docker como VM, backup→notificaciones, smoke Playwright en `e2e/`.
- **F15 + F15.1 (rama `feature/ssh-machines`)**: Remote Access — ssh/telnet/serial con terminal WS + pin de host key, SFTP con leases, transferencias streaming máquina↔máquina (máquina↔host también, tarjeta `host` in-process y solo admin), ZMODEM 3a/3b (descarga y subida `rz`) para telnet/serial, workspace Files en pestaña (una columna por defecto; subidas/bajadas entregan).
- **F15.2 (rama `feature/ssh-machines`)**: credenciales de la tarjeta **LOCAL** — cuenta + contraseña guardadas (misma registry 0600, clave `host`; es la única `builtin:true` que persiste), auto-login de una sola vez en el terminal (`/bin/login -p <user>` + feed user/password, rechazo/re-prompt/30 s devuelven el prompt intacto), `PUT /api/ssh-machines/host` + `POST /host/test` (probe de login real por PTY, sin tocar shadow) ambos solo admin con 404 al resto, y pestaña Settings en la tarjeta para admins.
- **F15.3 (rama `feature/ssh-machines`)**: las 4 peticiones del usuario al workspace Files — (1) barra de ruta editable (última miga = input; Enter navega; `/` = absoluto, lo demás relativo a la carpeta actual); (2) SIZE de carpetas vía `GET /api/ssh-machines/{id}/sftp/sizes?paths=…` (cap de 128 paths por petición, deadline 10 s, walk BFS profundidad 12 / 2 000 entradas, `≈` si el cap dispara, probe en segundo plano version-guarded en `paneModel.dirSizes`); (3) checkboxes por fila + select-all (indeterminate) que exponen el `pane.selected` ya existente — la copia/descarga parte de lo marcado, y la cola **Transfers** pausa/reanuda (gate en cada chunk de 64 KiB vía `waitIfPaused`/`putOpts.Gate`; el job pausado conserva lease y slot de semáforo; `Cancel` funciona desde parked; `Remove` y eviction lo tratan como vivo) y cancela; (4) subida de carpeta (input oculto `webkitdirectory directory multiple`) y descarga multi-selección = **un** job (`action: download-batch`, cap 64, raíces fusionadas en un árbol con nombres dedupeados `logs`/`logs (2)`, un solo tar.gz; el plan de lote lleva tope total de 50 000 entradas —truncado si se supera— y deadline de planificación de 10 min). Backend: `TransferManager.WaitAll(timeout)` (shutdown pair de `CancelAll`: un worker en cola solo ve el cancel cuando le toca) + `Handler.Close()` reordenado a CancelAll→Stop→WaitAll(10 s)→CloseAll.
- **F15.4 (rama `feature/ssh-machines`)**: los 3 bugs del workspace Files tras F15.3 — (1) **copia/descarga-carpeta sin barra en vivo**: `runCopy`/`runDownloadDir` solo hacían `broadcast()` en fail/final (el cliente con `total=0` no pinta barra) → tras el plan se publica el denominador **antes** del primer byte (bifuración de un solo fichero de `planFor` ahora fija también `FilesTotal=1`/`Total=size` — contrato antiguo «ausente = 1 fichero», cliente lee `files_total ?? 1`, comportamiento equivalente) y un `tick()` con tope de 300 ms tras cada `noteProgress` en el loop de chunks (`copyOne`/`saveLocal` reciben `onBytes` inyectado); (2) **cola no limpiable/borrable**: el `remove` por fila fallaba en silencio (404 tras eviction del servidor) y un frame tardío de SSE revivía la fila → store `remove()` optimista + tombstone `removed` (Set; `upsert` intercepta la resurrección; 404 = éxito; otro error revierte la fila y rethrow para el toast) + endpoint por lote `POST /api/machine-transfers/clear` (grupo capability `console`) → `TransferManager.RemoveFinished() (int, []string)` (solo done/error/canceled; salta running/paused y `inFetch`; `LocalFile` fuera del lock; devuelve (count, máquinas) para auditar cada extremo de la copia) + `files.clearFinished()` (local primero, un solo pass servidor, fallo restaura filas + rethrow) + botón **Clear N finished** en la cabecera de `TransferQueue` (Eraser, `files.queueClearFinished` {n}, `clearing` anti-reentrada); (3) **destino no refresca al terminar**: nadie volvía a preguntar al pane receptor → `$effect` en `FilesWorkspace` sobre `files.allJobs` (plain Set `seen` que **solo** admite jobs terminales — es lo que hace observable la transición running→done; copy matchea `dst_machine_id`+`dst_dir`, upload `machine_id`+`dirOf(remote_path(s))`; se miran **ambos** panes: en split el destino suele ser el panel *otro*; regla de afectación = `pane.path === dir` o el pane es ancestro de `dir` o raiz; un solo `refresh()` por pane por ejecución — el loader es version-guarded pero no deduplica in-flight).
- **F15.5 (rama `feature/ssh-machines`)**: los 2 bugs de «contador running caduco + render lento tras el tirón» — (1) **bucle de reconexión fantasma en `events.svelte.js`**: `_open()` cerraba la `EventSource` vieja antes de crear la nueva y `close()` dispara el `error` de la fuente muerta (contrato del navegador); el handler reaccionaba igual con su fuente muerta (`connected=false` + programar otro reconnect) que con la viva → ese reconnect cerraba el replacement sano 1 s después, cuyo fantasma programaba el siguiente… Como cada `open` reseteaba el backoff (`reconnectAttempts=0`), tras el primer tirón de red/servidor el cliente entraba en un ciclo permanente de ~1 s de cerrar/reabrir: cada gap perdía **para siempre** los eventos del momento (el hub no re-emite, buffer 16 non-blocking) — un `vm.state` shutoff perdido es el badge «1» que no se cura — y la chusma de sockets/handlers dejaba la UI pesada (el `open` repetido además re-corría `files.resync()` = 1 HTTP/s con el workspace abierto). Fix: el handler `error` ignora fuentes que ya no son `this._es` (`if (this._es !== es) return;`) y el `open` limpia el `_reconnectTimer` pendiente (el auto-reconnect del navegador ya lo tiene; el timer vivo le hubiera cerrado el socket sano nuevo); (2) **resync tras reconnect**: nada releía la lista de VMs en `open` (solo `files` lo hacía vía `onOpen`) → el `runningCount` del Sidebar (badge que **no** remonta al cambiar de ruta: Layout), la lista de VmList (nuevo `loadVMs({ silent: true })` — `loadVMs` ganó flag `silent` para que el resync no parpadee el spinner de página completa) y VmDetail (`load(true)`, ya silenciosa) registran `events.onOpen(resync)`; el fetch inicial del badge y el `onVmState` comparten ahora `refreshRunningCount()`. Tests: `eventsReconnect.dom.test.js` (4 tests con `EventSource` fake que replica el contrato `close()→error` asíncrono: el fantasma no programa, el `open` limpia el timer, `onOpen` dispara por cada (re)open y calla tras `off()`, disconnect deliberado sin timer colgado) + `routesMount.dom.test.js` precalienta los 23 chunks en `beforeAll` (el transform frío del grafo compartido — i18n 12k L, lucide, bits-ui — en una máquina cargada podía superar el timeout per-test de 30 s y fallar a las primeras rutas del fichero aunque el mount fuera sano). Queda pendiente (cosmético, no bloqueante): `stateToString` mapea `DOMAIN_EVENT_SHUTDOWN` a «shutoff» antes del `STOPPED` final.
- **F15.6 (rama `feature/ssh-machines`)**: copiar/pegar bidireccional en la consola de VM — implementado y **luego retirado íntegro a petición del usuario** (barra flotante/toast/cajón de pegado manual/endpoints `GET`+`POST /api/vms/{id}/clipboard` con sus rutas, tickets y auditoría, más el Ctrl+V auto-paste y el banner guest→host que ya había en HEAD). Motivo: en despliegues por HTTP plano `navigator.clipboard` no existe (API solo de secure context), así que los botones no podían funcionar en principio. Los métodos QGA `GuestSetClipboard`/`GuestGetClipboard` quedan en `libvirt/guestagent.go` (ya existentes en HEAD), ahora sin llamador en la capa API. Alternativas evaluadas si se quiere reabrir: servir por TLS (cero código, reviviría la Clipboard API) o delivery de texto vía `guest-file-open/write/close` del QGA (independiente de https).
- Último commit: `def9cb3 style(storage)` (punta de `main` y de la rama).

Deuda conocida (no bloqueante): restos de `exec.Command` sin `CommandRunner` en la capa API/host (`host_disks`, `host_netplan`, `host_updates`, `image_hub`, `services`, `system`, `console_serial`, `backupstore/net_mount`, `xz` de `appliances.go`) y pollers `setInterval` puntuales (AuditLog, Backup, Firewall, MultiView, Nodes, Status, VmList, SecurityJailTab) para señales que SSE no cubre.

## 7. Pipeline de verificación (obligatorio antes de commit / al actualizar código)

Antes de **commit** o de dar por buena cualquier actualización de código, ejecutar **todo** el pipeline de lo tocado (un cambio "pequeño" pasa el pipeline igual).

### 7.1 Backend Go (se tocó algún `.go`)
1. Cargar el skill **`go-ultimate`** — requerido para todo trabajo Go.
2. MCP **`go-quality`**: `install_tools` una vez (pre-warm de golangci-lint/govulncheck/nilaway). Bucle rápido: `run_lint`; pase final: `run_code_checks` (+ `run_nil_check`/`run_vuln_check` si se tocó auth/API/dependencias). En cada diagnóstico mirar **primero el campo `error`** (herramienta sin instalar); el resto son `file:line` accionables.
3. `cd backend && go build ./... && go vet ./... && go test -short ./...` — añadir `-race` si se tocó concurrencia, sshmachines/leases o el hub de eventos.
4. §3.4 aplicado a la diff: `exec` nuevo → `CommandRunner` + test con `MockCommandRunner`; sin `time.Sleep` rígido; sin `panic`.

### 7.2 Frontend (se tocó algún `.svelte` / `.svelte.js` / JS)
1. MCP **`svelte`** (obligatorio): `list-sections` → `get-documentation` de lo relevante; editar vía subagente **`svelte-file-editor`** (skill `svelte-code-writer`); pasar **`svelte-autofixer`** hasta devolver sin issues.
2. `cd frontend && npm test && npm run test:dom && npm run build`
3. `bash ../scripts/check-i18n.sh` (claves en en/es/ca) y `bash ../scripts/check-undefined-components.sh`.

### 7.3 Instalación / Docker (si se tocó)
`packaging/standalone/test-installer.sh` y/o `scripts/smoke.sh`.

### 7.4 Revisión final: opencode-review
1. `/review` — orquesta la revisión en 5 dimensiones (code-quality, security, performance, testing, documentation) sobre la diff actual (config en `.opencode/review.json`: idioma, reglas propias del proyecto, `max_diff_lines`).
2. Problemas **🔴 críticos** → arreglarlos con el agente **`review-fixer`** (edits mínimos) y repasar la dimensión.
3. Auto-revisión en idle: **ON** y persistida (`auto_on_idle: true`, cooldown 120s); estado/switch con `toggle_auto_review`. No crear un segundo mecanismo de revisión.

### 7.5 Cierre de fase
Actualizar este archivo (§6 + fecha) y `CHANGELOG.md` si hay cambio visible. No editar `frontend/dist/` ni `backend/internal/frontend/dist/` a mano.

## 8. Cómo seguir desarrollando

- Rama por fase desde `main`. Cambios locales, sin push salvo que se pida.
- Dev: `make dev-backend` + `make dev-frontend`. Build completo: `make build`. Limpieza: `make clean`.
- Convenciones: no re-monolitizar `vm-detail/` ni `domain_*.go`; handlers vía `compute.Backend`; errores sin `panic`; strings UI con clave en en+es+ca; `localStorage` solo con clave `webkvm.*`.
- Comandos útiles: `systemctl status webkvm` · `journalctl -u webkvm -f` · `curl -kfsS https://127.0.0.1:8080/api/health` · `webkvm-cli vms list` · `webkvm-cli --json vms list | jq .`

## 9. Archivos de referencia rápida

- Entradas: `backend/cmd/server/main.go`, `backend/cmd/cli/main.go`, `frontend/src/main.js`, `frontend/src/App.svelte`
- Router/API: `backend/internal/api/router.go`, `frontend/src/lib/router.svelte.js`
- Auth/seguridad: `backend/internal/auth/jwt.go|ticket.go|vnc_ticket.go|rbac.go`, `SECURITY.md`, `docs/runbooks/SECURITY-MODEL.md`
- Dominio: `backend/internal/libvirt/domain*.go`, `backend/internal/compute/backend.go`
- UI clave: `frontend/src/routes/VmDetail.svelte`, `frontend/src/lib/components/vm-detail/`, `TerminalPanel.svelte`, `MultiView.svelte`, `TaskDrawer.svelte`, `frontend/src/lib/stores/events.svelte.js`, `frontend/src/lib/i18n.svelte.js`
- Ops: `Makefile`, `Dockerfile`, `docker-compose.yml`, `scripts/webkvm.service`, `packaging/standalone/install.sh`, `docs/runbooks/RUNBOOKS.md`
- Historia: `CHANGELOG.md`, `AUDIT_IMPROVEMENTS.md`

## graphify

This project has a knowledge graph at graphify-out/ with god nodes, community structure, and cross-file relationships.

When the user types `/graphify`, use the installed graphify skill or instructions before doing anything else.

Rules:
- When graphify-out/graph.json exists, run `graphify query "<question>"` before your first grep, search or source-file read for a codebase question, and put this rule in every subagent prompt that explores code. Read raw files only for lines the graph or an earlier read already located. Use `graphify path "<A>" "<B>"` for relationships and `graphify explain "<concept>"` for focused concepts. These return a scoped subgraph, usually much smaller than GRAPH_REPORT.md or raw grep output.
- Dirty graphify-out/ files are expected after hooks or incremental updates; dirty graph files are not a reason to skip graphify. Only skip graphify if the task is about stale or incorrect graph output, or the user explicitly says not to use it.
- If graphify-out/wiki/index.md exists, use it for broad navigation instead of raw source browsing.
- Read graphify-out/GRAPH_REPORT.md only for broad architecture review or when query/path/explain do not surface enough context.
- After modifying code, run `graphify update .` to keep the graph current (AST-only, no API cost).
- A committed graphify-out/graph.json is rewritten whole on every build, so a large diff to it in a pull request is expected. Do not flag it as churn or suggest skipping the graph rebuild or its hooks.

<!-- antislop:start -->
## antislop
For UI, copy, people, mobile layout, or code comments work, read `antislop.md` (core) and then the skill for the task:
- UI / visual: `skills/antislop-ui/SKILL.md`
- Copy & text: `skills/antislop-copywriting/SKILL.md`
- People: `skills/antislop-human/SKILL.md`
- Mobile / responsive: `skills/antislop-layoutmobile/SKILL.md`
- Code comments: `skills/antislop-code/SKILL.md`
Before starting, follow the core's "Two Usage Modes" section in strict order: explicit session instruction first, then global preference, then ask. A session instruction always wins. For a resolved mode, say `antislop active: <mode> (session override).` or `antislop active: <mode> (global preference).` once before presenting findings or making edits, using the actual mode and source. Acknowledging the user's request without naming the source does not replace this notice.
Only an explicit choice of antislop during or after selects a session mode. A request to review, audit, or avoid file edits does not select a mode; read the global preference in that case. Another skill's mode does not select antislop's mode.
If the mode is unresolved, ask during/after and end the response; wait for the answer before any UI review, planning, or concept. For read-only tasks, put the active-mode notice only at the start of the final answer, never in progress messages. For editing tasks, announce before the first edit and omit it from the final answer.
To update antislop later: download `antislop.md` again, or run `npx antislop-ai --update` if it was installed as skill folders.
<!-- antislop:end -->

## UI/UX & Design Engineering Intelligence

When working on UI/UX components, visual design, transitions, or animations in WebKVM:
- **`ui-ux-pro-max`**: Design intelligence for visual styles, color contrast (4.5:1), responsive layout, accessibility semantics, and stack guidelines for Svelte 5 + Tailwind v4. Searchable datasets are available via `search.py` in `~/.config/opencode/skills/ui-ux-pro-max/scripts/`.
- **`emil-design-eng` / `animate`**: High-craft design engineering and micro-interactions based on Emil Kowalski's philosophy:
  - Never animate keyboard-driven high-frequency actions (e.g. quick search, Command Palette).
  - Use custom ease-out curves (`cubic-bezier(0.23, 1, 0.32, 1)`) for UI entries; avoid sluggish `ease-in`.
  - Buttons must provide subtle press feedback (`:active { transform: scale(0.97) }`).
  - Animate only `transform` and `opacity` to avoid layout thrashing and stay on the GPU compositor.
  - Duration budget: 150–250ms max for dropdowns and popovers; modals and drawers up to 300ms.
  - Popovers scale from their trigger origin (`transform-origin: var(--transform-origin)`).
