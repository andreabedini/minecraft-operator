# Minecraft operator: design

Status: agreed 2026-09-25. Phases 1 to 3 of section 15 are implemented and
tested (supervisor, operator core, management protocol); nothing has run on
the cluster yet. Section 15 is the implementation plan and tracks progress.

## 1. Goal

Run Minecraft servers on the homelab Kubernetes cluster so that the control
plane can restart, upgrade or crash without touching the game. Today Lodestone
runs every server as a child process of its own pod, so any restart of
Lodestone kills every world and needs a manual restart from the UI. That is the
problem this operator removes.

Secondary goals, in priority order:

1. Own the bootstrap. No prebuilt game image. The operator resolves and installs
   the server the way Lodestone does (vanilla, Fabric, Paper, Forge, others
   later), so nothing depends on a third party's image conventions.
2. Prefer the Minecraft Server Management Protocol (MSMP) over RCON. RCON goes
   away entirely.
3. Monitoring and logging are part of the design, not an afterthought, and they
   plug into the existing VictoriaMetrics, VictoriaLogs and Grafana stack.
4. Storage is deliberate: the install outlives the pod, the CR and the base
   image, and a base image update runs the same server.
5. Keep a path to a dashboard with the parts of Lodestone's UI that are used:
   console, file manager, start/stop, version change.

Non-goals for the first version: multi-node scheduling, proxies (Velocity),
multiple worlds per server, the dashboard itself, and support for versions
older than 1.21.9 (MSMP is required).

## 2. Context

Facts the design depends on. Details live in the homelab notes
(`~/homelab/notes/Kubernetes/Apps/Minecraft/`).

| Fact | Consequence |
|---|---|
| Single-node Talos, IPv6-only pod network, Cilium, NAT64/DNS64 for IPv4-only hosts | JVMs need `-Djava.net.preferIPv6Addresses=true`; egress policies are Cilium FQDN rules |
| Storage is OpenEBS ZFS-LocalPV; dataset-backed PVCs cannot be mounted by two pods, even read-only; `badssd-fs` has reclaim policy Delete | One pod per PVC; anything that needs files from the PVC goes through the supervisor; a Retain StorageClass is needed |
| Metrics: vmagent scrapes `VMServiceScrape`/`VMPodScrape`; logs: Vector agent ships container stdout to VictoriaLogs | The server must log to stdout; the operator creates scrape objects |
| Current world: Fabric on Minecraft 26.x, Geyser + Floodgate for Bedrock, a `clat` native sidecar with `NET_ADMIN` for Xbox joins, `lbipam` shared LoadBalancer IP | The pod template must accept extra sidecars, sysctls, volumes and Service annotations |
| The existing world lives on PVC `lodestone-data` at `instances/BediniCraft-b86164d8/` | First cutover must adopt an existing directory, not re-provision |
| The dirien world-stats exporter is a sidecar today and its RCON `list` call breaks after restarts; shipping a new exporter build restarts the game (homelab roadmap D9) | Exporter becomes its own pod reading files through the supervisor |

## 3. Architecture

```
                 ┌──────────────────────────────┐
                 │ operator (Deployment)         │
                 │  controller-runtime, Go       │
                 │  all Minecraft knowledge      │
                 └───┬───────────────┬───────────┘
      gRPC (connect) │               │ MSMP (WebSocket JSON-RPC)
                     ▼               ▼
        ┌────────────────────────────────────────────┐
        │ server pod (one per MinecraftInstance CR)     │
        │  init: copy supervisor binary → emptyDir    │
        │  main: eclipse-temurin:<major>-jre          │
        │        PID 1 = supervisor                   │
        │           └─ java … -jar server.jar nogui   │
        │  volumes: PVC /data (whole install)         │
        │  optional user sidecars (e.g. clat)         │
        └────────────────────────────────────────────┘
                     ▲
      gRPC (files)   │
        ┌────────────┴─────────────┐   ┌─────────────────────┐
        │ stats exporter (Deploy)  │   │ backup Job / CronJob │
        └──────────────────────────┘   └─────────────────────┘
```

Roles:

- **Operator** (brain). Watches `MinecraftInstance` CRs. Resolves versions,
  URLs, hashes and the Java major. Drives the supervisor with downloads, runs
  and writes. Talks MSMP to the running server for state, players and settings.
  Exposes metrics and status. Later, backs the dashboard and proxies console and
  file access so browsers never reach a pod directly.
- **Supervisor** (agent). A static Go binary that knows nothing about
  Minecraft. It manages one directory and one long-running process, and offers
  a small gRPC API: download a file, run a command, set and persist the launch
  spec, start/stop/restart, console stream, file access. It autostarts the
  server on boot from the persisted spec so the game survives operator
  downtime and node reboots.
- **Server pod**. Stock Eclipse Temurin JRE image. Everything else lives on the
  PVC. Swapping the image tag is a pod restart on the same data.
- **Stats exporter** and **backup jobs** are ordinary pods that read the
  install through the supervisor's file API. Nothing else mounts the PVC.

## 4. Custom resource

API group `minecraft.bedini.au`, version `v1alpha1`, kind `MinecraftInstance`.

```yaml
apiVersion: minecraft.bedini.au/v1alpha1
kind: MinecraftInstance
metadata:
  name: bedinicraft
  namespace: minecraft
spec:
  version: "26.3"                    # Minecraft version; change = upgrade flow
  flavour:
    fabric:                          # exactly one of vanilla|fabric|paper|forge
      loaderVersion: "0.19.5"        # optional; resolved to latest stable if absent
      installerVersion: ""           # optional
    # paper: {build: 41, channel: stable}   # channel: stable|beta|alpha (least mature accepted)
  java:
    image: ""                        # full image reference; when empty the operator
                                     # derives it from the resolved Java major with its
                                     # image template (default eclipse-temurin:{major}-jre)
  jvm:
    minMemoryMiB: 2048
    maxMemoryMiB: 6144
    extraArgs: []                    # appended verbatim before the launch target
    env:
      JAVA_TOOL_OPTIONS: "-Djava.net.preferIPv6Addresses=true"
  mods:
    - name: fabric-api
      modrinth: {project: P7dR8mSH, version: "0.158.0+26.2"}
    - name: geyser
      url: https://…/Geyser-Fabric.jar
      digest: {sha512: "…"}
  configFiles:
    - path: server.properties
      configMapKeyRef: {name: bedinicraft-properties, key: server.properties}
      merge: properties              # key-wise merge; reserved keys always win
    - path: config/Geyser-Fabric/config.yml
      configMapKeyRef: {name: geyser-config, key: config.yml}
    - path: config/flan/flan_config.json
      secretKeyRef: {name: flan-config, key: flan_config.json}
  storage:
    size: 20Gi
    storageClassName: badssd-fs-retain
    # existingClaim: lodestone-data    # adoption path, mutually exclusive
    # subPath: instances/BediniCraft-b86164d8
    retainOnDelete: true
  service:
    game:
      type: LoadBalancer
      annotations:
        lbipam.cilium.io/sharing-key: lodestone
        lbipam.cilium.io/ips: "2403:580e:e231:3500::100"
      labels: {bgp: advertise}
    extraPorts:
      - {name: voice, port: 24454, protocol: UDP}
      - {name: bedrock, port: 19132, protocol: UDP}
  autostart: true                    # supervisor relaunches on boot
  stopped: false                     # true stops the server but keeps the pod
  restartPolicy: manual              # manual | automatic (on staged config drift)
  upgrade:
    backupBeforeUpgrade: true
    allowDowngrade: false
  metrics:
    statsExporter: true              # separate pod, file API
    modEndpoint: {port: 9225, path: /metrics}   # optional in-process metrics mod
  supervisor:
    version: ""                      # pinned per CR; empty = operator default at creation
  resources:
    requests: {cpu: "500m", memory: 2Gi}
    limits: {memory: 8Gi}
  podOverrides: {}                   # strategic merge patch on the pod template
                                     # (sidecars, sysctls, volumes, securityContext)
status:
  conditions:
    - type: SupervisorReady          # gRPC reachable
    - type: Installed                # install manifest matches resolved spec
    - type: Running                  # java process alive (supervisor)
    - type: Ready                    # MSMP server/status.started == true
    - type: ConfigDrift              # staged writes pending a restart
    - type: UpgradeBlocked           # mod incompatibility or downgrade
  resolved:
    version: "26.3"
    javaMajor: 25
    loaderVersion: "0.19.5"
    installerVersion: "1.1.0"
    serverJar: {path: server.jar, sha256: "…"}
    mods:
      - {name: fabric-api, file: mods/fabric-api-0.158.0+26.2.jar, sha512: "…"}
  supervisorVersion: "0.1.0"
  managementProtocolVersion: "3.1.0"
  players: {online: 3, max: 20}
  lastSave: "2026-09-25T10:12:00Z"
  upgrade: {phase: "", progress: 0}
```

Rules:

- The operator records what it resolved in `status.resolved` and only
  re-resolves when the relevant spec field changes. Lodestone re-resolves
  "latest" implicitly and its recorded loader version went stale; this avoids
  both.
- `server.properties` is a config file like any other, with `merge: properties`
  so the operator merges keys instead of replacing the file. The server
  rewrites the file on every start (sorted keys, defaults added), so drift for
  this file is detected key-wise, not byte-wise. A subset of keys is reserved
  and always set by the operator regardless of the ConfigMap: `server-port`,
  `enable-rcon=false`, and the `management-server-*` keys. If no config file
  entry exists for it, the operator still merges the reserved keys into
  whatever file is there. Lodestone rewrites the whole file from its manifest
  and drops unknown keys; this design never drops a key.
- The management protocol is not exposed in the spec. It is an
  operator-to-supervisor contract: the server binds it to localhost, the
  operator reaches it through the supervisor's tunnel (section 7), and nothing
  else can reach it.
- `podOverrides` exists because the current deployment needs a `clat` sidecar,
  `sysctls`, a `/dev/net/tun` hostPath and a privileged namespace. The
  operator does not model those; it applies the patch. The CLAT stays a
  sidecar rather than a supervisor-managed process: it needs real uid 0 and
  `NET_ADMIN` to create the tun device, and putting that in the JRE container
  would run the game, and its mods, in a privileged container. A sidecar keeps
  the privilege in a container that runs nothing else. If `podOverrides`
  proves too clumsy, a first-class `spec.network.clat` that generates the same
  sidecar is the better fix.

## 5. Supervisor

### 5.1 Scope

Minimal and generic. It manages one directory (`/data`) and one supervised
process. It does not know what Minecraft, Fabric or a mod is.

Library decision (phase 1): `github.com/go-proc/supervisor` and
`github.com/go-proc/respawn` were considered (pure Go, BSD-3, PID 1 subreaper,
pluggable `Runtime` interface with `Create`, `Start`, `State`, `Kill`,
`Delete`, restart state machine with backoff and anti-thrash) and not adopted.
They leave stdio piping to the `Runtime` implementer, which is most of what
this supervisor does, so they would have covered reaping and the restart
policy only. Those two pieces are about eighty lines here, and the reaper has
a requirement the library does not express: it must leave the managed child
to the manager's own `Wait` (done by peeking with `waitid` and `WNOWAIT`
before reaping). The libraries were also brand new at the time (first
published August 2026, no tagged release), which is a poor fit for a PID 1
component. The implementation is plain `os/exec` plus a `SIGCHLD` reaper. The
evaluation was from their documentation and README, not from running them.

- PID 1 of the JRE container. Reaps children.
- Spawns the launch spec as a child, pipes stdin, stdout and stderr.
- Passes the child's stdout and stderr through to its own stdout unchanged, so
  container logs, and therefore Vector and VictoriaLogs, see the server log.
- Keeps a ring buffer of recent console lines and streams new lines to console
  clients. Accepts command lines and writes them to the child's stdin.
- On SIGTERM: writes `stop` to stdin, waits for exit up to the grace period,
  then SIGKILLs. `terminationGracePeriodSeconds` on the pod is set accordingly
  (default 120 s).
- Persists the launch spec and an install manifest under `/data/.supervisor/`.
  On boot, if a launch spec exists and `autostart` is true, it launches without
  waiting for the operator.
- Serves the gRPC API on a pod port, authenticated with a bearer token mounted
  from a Secret.

### 5.2 Injection

The operator ships the supervisor as a static binary (`CGO_ENABLED=0`) in a
minimal image. The server pod has an init container using that image which
copies the binary into an emptyDir. The main container mounts the emptyDir and
runs the binary as its command. The JRE image is unmodified.

The supervisor image tag is pinned per CR in `status.supervisorVersion`. An
operator upgrade does not change running pods; a supervisor roll happens when
`spec.supervisor.version` changes or when the operator is asked to roll.

### 5.3 API

gRPC, served with connect-go so the same port answers gRPC, gRPC-Web and
Connect's JSON-over-HTTP. Server reflection is enabled so `grpcurl` works
without the proto on hand. Streaming is used for console, file transfer and
progress.

```protobuf
service Supervisor {
  // Process
  rpc Status(StatusRequest) returns (StatusResponse);          // installed, running, pid, uptime, launch spec hash
  rpc SetLaunch(LaunchSpec) returns (SetLaunchResponse);       // persisted; cmd, args, cwd, env, autostart
  rpc Start(StartRequest) returns (StartResponse);
  rpc Stop(StopRequest) returns (StopResponse);                // "stop" on stdin, wait, kill after timeout
  rpc Restart(RestartRequest) returns (RestartResponse);
  rpc Console(stream ConsoleInput) returns (stream ConsoleLine); // replay N lines, then live; input lines → stdin

  // Install
  rpc Download(DownloadRequest) returns (stream Progress);     // url, path, digest{algo,value} (optional)
  rpc Run(RunRequest) returns (stream RunOutput);              // one-shot: cmd, args, cwd, env; ends with exit code

  // Files
  rpc ListFiles(ListRequest) returns (ListResponse);           // path, recursive, with size/mtime/digest on request
  rpc ReadFile(ReadRequest) returns (stream FileChunk);        // offset/length supported
  rpc WriteFile(stream FileChunk) returns (WriteResponse);     // first chunk carries path + mode (staged|immediate)
  rpc DeleteFile(DeleteRequest) returns (DeleteResponse);      // staged|immediate
  rpc ApplyStaged(ApplyRequest) returns (ApplyResponse);       // move staged tree into place; refused while running
  rpc Archive(ArchiveRequest) returns (stream FileChunk);      // tar stream; consistent=true runs save-off/save-all flush/save-on

  // Tunnel
  rpc Tunnel(stream TunnelFrame) returns (stream TunnelFrame); // first frame names a target from the launch spec's
                                                               // allowlist (e.g. "management" → 127.0.0.1:25585);
                                                               // then raw bytes both ways
}
```

`Tunnel` is what carries the management protocol. The supervisor does not
speak JSON-RPC; it forwards bytes to a localhost port named in the launch
spec. The operator wraps the stream as a `net.Conn` and runs its WebSocket
client over it. One channel, one token, one network policy port, and the
management server never listens on a pod address.

### 5.4 Guard rails

- Every path is resolved and checked to stay inside `/data`, including through
  symlinks.
- `Download`, `Run`, `ApplyStaged`, immediate `WriteFile` and `DeleteFile` are
  refused while the child process is running. Staged writes are the only
  mutation allowed then.
- `Download` accepts https only. An optional host allowlist is passed at pod
  creation. Files are streamed to a temp file, the digest is verified, and the
  file is renamed into place. Digest algorithms: sha1, sha256, sha512. A request
  without a digest is allowed and the response reports the observed digest.
- `Run` is arbitrary command execution inside the container. This is accepted:
  the operator is the only client, the container runs as uid 1000 with all
  capabilities dropped, and the blast radius is the instance PVC.
- Read-only is enforced by which RPCs a token may call, not by convention. Two
  tokens exist per instance: a full token for the operator and a read-only
  token (Status, ListFiles, ReadFile, Archive) for the exporter and backups.
- `Archive` with `consistent=true` sends `save-off`, `save-all flush`, streams,
  then `save-on`, and restores `save-on` on any error.
- `Tunnel` only connects to targets listed in the launch spec, all on
  loopback. It is full-token only.

### 5.5 Persistence on the PVC

```
/data/.supervisor/launch.json      # last SetLaunch, including autostart
/data/.supervisor/manifest.json    # files installed via Download/Run/Apply with digests
/data/.supervisor/staged/…         # pending writes, mirrors the /data tree
```

The CR status is authoritative for what the operator resolved. The manifest
lets the operator reconcile a PVC it has never seen (a recreated CR, or the
adopted Lodestone directory) by listing files with digests and filling only
the gaps.

## 6. Provisioning (operator side)

The operator turns a spec into a sequence of `Download`, `Run`, `WriteFile`,
`SetLaunch`, `Start`. First install on an empty PVC and a version change are
the same loop.

### 6.1 Version and jar resolution per flavour

Taken from Lodestone's implementation, with the fixes noted.

| Flavour | Versions | Jar |
|---|---|---|
| Vanilla | Mojang manifest `https://piston-meta.mojang.com/mc/game/version_manifest_v2.json` | per-version JSON `downloads.server.url` + `sha1` |
| Fabric | `https://meta.fabricmc.net/v2/versions/game`; loader `…/versions/loader/{mc}`; installer `…/versions/installer` | launcher jar `https://meta.fabricmc.net/v2/versions/loader/{mc}/{loader}/{installer}/server/jar`. No published hash; observed digest recorded. Downloads the vanilla jar and libraries on first start into `.fabric/` and `libraries/` |
| Paper | Fill v3: `https://fill.papermc.io/v3/projects/paper/versions/{v}/builds` (v2 was sunset in 2026, HTTP 410) | newest build whose channel (`STABLE`, `BETA`, `ALPHA`) is at least the spec's; the download entry `server:default` carries an absolute URL and sha256 |
| Forge | `https://files.minecraftforge.net/net/minecraftforge/forge/promotions_slim.json` (recommended build, not the newest as Lodestone does) | installer `https://maven.minecraftforge.net/net/minecraftforge/forge/{build}/forge-{build}-installer.jar`, then `Run: java -jar forge-installer.jar --installServer /data` |
| NeoForge, Quilt | later; same shape | |

Defaults when the spec leaves a version unset: Fabric loader is the highest
with `loader.stable && intermediary.stable`; Fabric installer is the first
stable entry (Lodestone's comparator has a panic bug here); Paper build is the
newest in the requested channel or better; Forge build is the recommended one.

Artifacts without a publisher digest (the Fabric launcher jar) are compared
against the sha256 the operator observed when it first fetched or adopted
them, recorded in `status.resolved`. A file that is present with no known
digest is adopted as is, never re-downloaded.

### 6.2 Java

The Mojang per-version JSON carries `javaVersion.majorVersion`. Missing means
8; 16 is mapped to 17 because Temurin has no 16. Fabric, Paper and Forge use the
Java of their underlying Minecraft version. The image is `spec.java.image`
when set, otherwise the operator's image template applied to the major
(default `eclipse-temurin:{major}-jre`, an operator flag, since the tag scheme
is Temurin's and other JRE images use different ones). A Java major change is
a pod roll; nothing else about the install changes.

### 6.3 Launch spec

Working directory `/data`.

```
java -Xms{min}M -Xmx{max}M {extraArgs…} {target} nogui
```

Target by flavour: vanilla, Paper and Fabric use `-jar server.jar`. Forge
1.17 and later uses `@libraries/net/minecraftforge/forge/{build}/unix_args.txt`
with no `-jar`. Older Forge is out of scope.

Environment comes from `spec.jvm.env`. The IPv6 preference flag belongs there
and is applied to the installer `Run` as well.

### 6.4 Files written by the operator

- `eula.txt` with `eula=true`. Accepting the EULA is a conscious act; the CRD
  documents that creating a `MinecraftInstance` implies it.
- `server.properties`: read the file in place, merge the keys from the config
  file entry (if any) and the reserved keys, write back. Never written while
  running; staged and applied on the next restart.
- `mods/` (`plugins/` for Paper): the jars in `spec.mods`, named by the
  upstream file name. Jars the operator installed earlier and that are no
  longer in the spec are removed. Jars the operator never installed (an
  adopted directory, or a hand-copied mod) are never deleted; they are
  reported as an `UnmanagedMods` event so a typo in the spec cannot destroy
  a world's mods. Modrinth entries resolve through
  `https://api.modrinth.com/v2/project/{id}/version/{version}` for the file
  URL and sha512, and the version's `game_versions` and `loaders` are checked
  against the spec.
- `configFiles`: content from ConfigMaps or Secrets, written as staged files.
  Drift between the desired content and the file in place sets the
  `ConfigDrift` condition; `restartPolicy: automatic` turns that into a
  supervisor restart, `manual` waits for a human.

## 7. Management protocol

Minecraft 1.21.9 and later ship a management server: WebSocket carrying
JSON-RPC 2.0, discoverable with `rpc.discover`, authenticated with
`Authorization: Bearer <secret>`. It is the operator's source of truth for
server state and replaces RCON.

Operator-set properties:

| Key | Value | Why |
|---|---|---|
| `management-server-enabled` | `true` | |
| `management-server-host` | `127.0.0.1` | reached only through the supervisor tunnel |
| `management-server-port` | `25585` (operator constant, one instance per pod) | default `0` picks a random port |
| `management-server-secret` | generated by the operator into the instance Secret | the server would otherwise generate one and write it to the file |
| `management-server-tls-enabled` | `false` | the server refuses to start with TLS on and no keystore; loopback plus the supervisor's token replaces it |
| `status-heartbeat-interval` | operator default (15 s) | periodic `server/status` notifications |
| `enable-rcon` | `false` | RCON is retired |

None of this is in the CR spec. The operator connects by opening a `Tunnel`
stream to the `management` target and speaking WebSocket JSON-RPC over it.

Used by the operator:

- `minecraft:server/status` for `Ready`; since protocol 3.0.0 (Minecraft
  26.2) the management server starts before the world loads, so the connection
  is up during long world upgrades and the startup probe can rely on it.
- Notifications `server/started`, `server/stopping`, `server/saving`,
  `server/saved`, `players/joined`, `players/left`, `gamerules/updated` and,
  from 3.1.0 (26.3), `world/upgrade_*` for status, events and metrics.
- `minecraft:server/save` before backups and stops, `minecraft:server/stop`
  as the polite stop path when MSMP is up (stdin `stop` is the fallback).
- `minecraft:serversettings/*` for runtime settings later, when the dashboard
  needs them.

Not available through MSMP and therefore done through the supervisor console:
arbitrary commands and console output. Mojang has removed log lines that
server software used to watch (26.3 changelog) and recommends the protocol,
which is why readiness comes from MSMP and not from the `Done` log line.

Supported flavours: vanilla, Fabric (vanilla jar underneath), Paper (a startup
crash with the protocol enabled was fixed in April 2026), NeoForge (with a
first-party extension API since 26.2). Forge is unverified.

## 8. Lifecycle flows

### 8.1 Create

1. Validate the spec. Resolve version, Java major, jar, mods. Set
   `status.resolved`.
2. Create the PVC (unless `existingClaim`), the instance Secret (supervisor
   tokens, management secret), the Services, the network policies, the
   `VMPodScrape`, and the pod (via a Deployment with `Recreate`, replicas 1).
3. Wait for `SupervisorReady`.
4. `ListFiles` with digests; compare to `status.resolved` and the manifest.
5. For each missing or mismatching artifact: `Download` (or `Run` for the
   Forge installer).
6. `WriteFile` eula, properties, config files. `ApplyStaged`.
7. `SetLaunch`, `Start`.
8. Connect MSMP through the tunnel. `Ready` when `server/status.started` is
   true.

### 8.2 Adopt an existing directory

Same as create with `storage.existingClaim` and `subPath`. Step 4 finds the
jar, mods and world already present and step 5 downloads nothing. The
operator's owned properties keys are merged into the existing file. Lodestone
must be scaled to zero first; two processes on one world is never allowed.

### 8.3 Restart

`Restart` on the supervisor. The pod stays. Used for config drift, mod changes
and same-Java version changes.

### 8.4 Version change

Triggered by a change to `spec.version` or the flavour's pinned versions.

1. Resolve the new jar, hash and Java major.
2. Validate every Modrinth mod against the new game version and loader. On a
   mismatch set `UpgradeBlocked` and stop, unless the spec carries
   `upgrade.force: true`.
3. Refuse downgrades unless `allowDowngrade`.
4. If `backupBeforeUpgrade`, run a backup Job that calls `Archive` with
   `consistent=true` and stores the tar (destination is a later decision; a
   PVC on the NAS class is the obvious one).
5. `Stop`.
6. If the Java major changed, update the Deployment and wait for the new pod's
   `SupervisorReady`.
7. `Download` the new jar and any changed mods; remove stale mod jars;
   `ApplyStaged`; `SetLaunch`; `Start`.
8. Follow `world/upgrade_*` notifications into `status.upgrade`. `Ready` when
   started.
9. Update `status.resolved`.

### 8.5 Delete

The Deployment, Services, Secret, policies and scrape objects are garbage
collected through owner references. The PVC is kept unless
`retainOnDelete: false`. A re-created instance adopting the claim gets a new
management secret and rewrites it into `server.properties`.

### 8.6 Pod probes

The supervisor port (9800) drives the startup and liveness probes: the pod is
alive when the supervisor answers, whatever the game does. The game port
drives readiness, so a stopped server drops out of the game Service while the
supervisor Service, which publishes not-ready addresses, stays reachable.

## 9. Storage

- One PVC per server holding the whole install: jar, libraries, Fabric cache,
  mods, config, world, properties, logs, supervisor state. The container image
  contributes only Java.
- StorageClass `badssd-fs-retain`: same ZFS-LocalPV parameters as `badssd-fs`,
  `reclaimPolicy: Retain`. Added in the homelab repo. The operator never
  touches PersistentVolumes.
- With `retainOnDelete: true` (the default) the operator sets no owner
  reference on the PVC, so deleting the instance leaves the claim. With
  `false` the PVC carries an owner reference and is garbage collected. No
  finalizer is needed. Retain on the StorageClass protects against
  accidental PVC deletion; it leaves a Released PV that needs manual
  re-adoption, which is the same procedure the NAS class already uses.
- Only the server pod mounts the PVC. Everything else reads through the
  supervisor. This is what removes the dataset-sharing problem.
- World snapshots: ZFS snapshots of the dataset via `VolumeSnapshot` remain
  possible and are cheap; the `Archive` RPC is for portable backups.

## 10. Networking

Per instance:

- `Service {name}-game`: LoadBalancer, TCP 25565 plus `extraPorts`,
  annotations and labels from the spec (lbipam sharing key, BGP label).
- `Service {name}-supervisor`: ClusterIP, the gRPC port only. The management
  protocol rides the tunnel.
- `Service {name}-metrics` or pod port for the optional metrics mod.

CiliumNetworkPolicies:

- Ingress to the supervisor port (9800) from the operator, the stats exporter
  and backup jobs only. There is no other management port.
- Ingress to the game ports from the world.
- Egress from the server pod, as FQDN rules: `piston-meta.mojang.com`,
  `piston-data.mojang.com`, `launchermeta.mojang.com`, `libraries.minecraft.net`,
  `meta.fabricmc.net`, `maven.fabricmc.net`, `api.papermc.io`,
  `files.minecraftforge.net`, `maven.minecraftforge.net`, `api.modrinth.com`,
  `cdn.modrinth.com`, plus the session servers the game already needs
  (`sessionserver.mojang.com`, `api.mojang.com`, `api.minecraftservices.com`).
  Geyser's Xbox hosts stay in the user's own policy.

The operator namespace needs egress to the same metadata hosts for version
resolution.

## 11. Monitoring and logging

Logs. The supervisor passes the server's stdout through. The Vector agent ships
it to VictoriaLogs with namespace, pod and container fields. Lodestone swallows
the child's output into its own buffer today, so this is new coverage. A VRL
transform can split timestamp, thread and level later. The console ring buffer
in the supervisor is for the dashboard, not for retention.

Metrics, three layers:

1. **Operator.** Per-instance gauges from MSMP, labelled `namespace` and
   `name`: `minecraft_instance_started`, `minecraft_instance_players_online`,
   `minecraft_instance_last_save_timestamp_seconds`,
   `minecraft_instance_world_upgrade_progress` and the counter
   `minecraft_instance_player_joins_total`, plus controller-runtime's
   reconcile metrics, all on the operator's metrics endpoint. A per-instance
   watcher goroutine keeps a management connection through the tunnel,
   re-queries `server/status` on `players/joined` and `players/left` (exact
   counts, not deltas), records `server/saved`, follows `world/upgrade_*`,
   emits Kubernetes Events for joins, leaves, start, stop and upgrades, and
   triggers a reconcile so status follows within seconds. This closes the gap
   where the exporter's RCON `list` breaks after restarts.
2. **Stats exporter.** The patched dirien exporter runs as its own Deployment
   created by the operator when `metrics.statsExporter` is true. Its file
   reads are replaced by `ReadFile` and `ListFiles` calls with the read-only
   token. Stats and usercache files are small and written atomically enough
   for periodic reads. This is roadmap D9 resolved without storage migration.
3. **In-process mod** (optional). TPS, MSPT, JVM, entities and chunks only
   exist inside the server. If `spec.metrics.modEndpoint` is set, the operator
   adds the port to the scrape. The mod itself is just another pinned entry in
   `spec.mods`. A bespoke mod that also exports per-player statistics from the
   live stats counters, falling back to the files for offline players, is the
   long-term replacement for layer 2.

Scraping: one `VMPodScrape` per instance covering the operator-exported
metrics (served by the operator, labelled by instance), the stats exporter,
and the mod port when present. Kubernetes Events on every state transition.
Grafana dashboard provisioned from a ConfigMap later; the hand-imported one
stays until then.

## 12. Security

- Supervisor tokens: two per instance in the instance Secret, full and
  read-only, checked per RPC. Rotated by the operator on request.
- MSMP secret: generated by the operator, 40 alphanumeric characters as the
  server expects, stored in the instance Secret, written into
  `server.properties`. The server listens on loopback only; TLS off; access
  only through the supervisor tunnel with the full token.
- RCON off. The pending RCON password rotation becomes moot at cutover.
- Server container: `runAsUser 1000`, `runAsNonRoot`, all capabilities dropped,
  `seccompProfile RuntimeDefault`. `podOverrides` can loosen this for
  sidecars, as `clat` needs.
- The supervisor's `Run` and `Download` are limited to the operator's token
  and refused while the server runs.
- Browsers never reach a pod. The dashboard, when it exists, talks to the
  operator, which proxies console and files.

## 13. Migration from Lodestone

1. Deploy the operator. Add the Retain StorageClass.
2. Create a `MinecraftInstance` with `existingClaim: lodestone-data`, the
   instance `subPath`, the current mods listed with their versions, the Geyser
   and Flan config as ConfigMaps, and the `clat` sidecar in `podOverrides`.
   Keep `autostart: false` and do not start.
3. Scale Lodestone to zero. Start the server through the operator. Verify
   players can join over Java and Bedrock, and that metrics and logs arrive.
4. Move the Services' shared IP and BGP label to the operator-created
   Services. Retire the Lodestone manifests (the Flux Kustomization prunes).
5. Later, copy the install to a dedicated PVC on the Retain class and switch
   the CR from `existingClaim` to its own claim.

## 14. Open points and things to verify

- Whether MinecraftForge exposes the management server; NeoForge and Paper do.
- NeoForge and Quilt metadata URLs.
- Backup destination and retention.
- Whether `Archive` should also snapshot via the ZFS CSI when available, rather
  than tar.
- Operator implementation (decided in phase 2, see section 16): Go with
  controller-runtime and controller-gen, kubebuilder conventions without the
  kubebuilder or Operator SDK scaffold.

## 15. Phasing

1. **Supervisor** (done 2026-09-25): proto, connect-go server, process
   management, console, download, run, files, staged writes, persistence,
   autostart, tunnel. Testable alone with `grpcurl` against a local directory.
2. **Operator core** (done 2026-09-25, not yet run on a cluster): CRD,
   Deployment/PVC/Secret/Service creation, vanilla/Fabric/Paper/Forge
   resolution and provisioning, mods and config files, start/stop, adoption
   of an existing directory. Ready mirrors Running until phase 3.
3. **MSMP** (done 2026-09-25): JSON-RPC client over the tunnel, `Ready`
   from `server/status`, players and protocol version in status, the
   notification watcher, events and operator metrics.
4. **Mods, config files, version change** with the upgrade guards and backup.
5. **Monitoring**: stats exporter over the file API, `VMPodScrape`, log check,
   Grafana provisioning.
6. **Cutover** of the live world, following section 13.
7. **Paper and Forge**, then the dashboard.

## 16. Implementation notes

- **Language and framework.** Go, `sigs.k8s.io/controller-runtime` for the
  manager, client cache, reconcile loop and metrics, and `controller-gen` for
  deepcopy, the CRD and RBAC from markers. The repo follows the kubebuilder
  layout (`api/v1alpha1`, `internal/controller`, `config/`, `cmd/`) so the
  markers and tooling work, but neither `kubebuilder init` nor the Operator
  SDK scaffold was run: both generate a Makefile and boilerplate around the
  same library, and the Operator SDK's additions (OLM bundles, scorecard,
  OperatorHub packaging) target publishing an operator, not running one on a
  Flux-managed homelab cluster. Go also keeps one module with the supervisor
  and one generated Connect client shared by operator, exporter and tests.
- **Testing.** No envtest or cluster. The controller tests use the
  controller-runtime fake client for the API server, the real supervisor
  in-process over a temp directory, fake publisher endpoints, a fake `java`
  script on `PATH`, and a fake management protocol server on a loopback port
  that the supervisor tunnel connects to. Everything runs under `-race`.
- **Plan cache.** Resolving a spec hits the publishers, so plans are cached
  per instance generation and management secret. Observed digests from status
  are merged into a fresh plan so a re-resolved Fabric launcher jar is not
  re-downloaded.

