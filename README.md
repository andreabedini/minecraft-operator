# minecraft-operator

A Kubernetes operator for Minecraft servers, built so that the control plane
can restart without touching the game. See [docs/design.md](docs/design.md)
for the design.

## Layout

| Path | What |
|---|---|
| `docs/design.md` | The design document |
| `proto/supervisor/v1/` | The supervisor's gRPC API |
| `gen/` | Generated protobuf and Connect code (`buf generate`) |
| `internal/supervisor/` | The supervisor: process management, console, files, downloads, tunnel |
| `internal/supervisorclient/` | The operator's client for one supervisor |
| `internal/upstream/` | Version and artifact resolution: Mojang, Fabric, Paper (Fill v3), Forge, Modrinth |
| `internal/plan/` | Spec to downloads, installers, launch spec, reserved properties |
| `internal/controller/` | The MinecraftInstance reconciler and resource builders |
| `api/v1alpha1/` | The MinecraftInstance CRD types |
| `config/` | CRD, RBAC and operator Deployment (`kubectl apply -k config/default`) |
| `cmd/supervisor/`, `cmd/operator/` | The two binaries |
| `.ko.yaml` | Image builds for both binaries with [ko](https://ko.build) |
| `.github/workflows/images.yaml` | Tests, image publishing to ghcr.io, releases |

## Install

The operator and supervisor images are published to ghcr.io for
`linux/amd64` and `linux/arm64`:

```
ghcr.io/andreabedini/minecraft-operator/operator
ghcr.io/andreabedini/minecraft-operator/supervisor
```

| Tag | Built from |
|---|---|
| `vX.Y.Z`, `vX.Y`, `latest` | a release tag |
| `vX.Y.Z-rc1` and similar | a pre-release tag, which moves no other tag |
| `main`, `sha-<short>` | every push to `main` |

Pin the operator to a full version such as `vX.Y.Z`. The operator pulls the
supervisor tagged with its own full version, whichever tag you picked it by.

The operator uses the supervisor image with its own version tag unless
`SUPERVISOR_IMAGE` (or `spec.supervisor.image` on an instance) says
otherwise. Pinning the operator image therefore pins the supervisor too.

### From a release

Each release has an `install.yaml` with the CRD, RBAC, namespace and
Deployment, with both images pinned by digest:

```sh
kubectl apply -f https://github.com/andreabedini/minecraft-operator/releases/latest/download/install.yaml
```

Replace `latest/download` with `download/vX.Y.Z` for a specific release.

### With Kustomize

`config/default` is a Kustomize base. Point at a tag and set the matching
image tag:

```yaml
# kustomization.yaml
resources:
  - https://github.com/andreabedini/minecraft-operator//config/default?ref=vX.Y.Z
images:
  - name: ghcr.io/andreabedini/minecraft-operator/operator
    newTag: vX.Y.Z
```

Use `?ref=main` with `newTag: main` to follow the main branch.

### With Flux

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: minecraft-operator
  namespace: flux-system
spec:
  interval: 1h
  url: https://github.com/andreabedini/minecraft-operator
  ref:
    tag: vX.Y.Z
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: minecraft-operator
  namespace: flux-system
spec:
  interval: 1h
  sourceRef:
    kind: GitRepository
    name: minecraft-operator
  path: ./config/default
  prune: true
  wait: true
  images:
    - name: ghcr.io/andreabedini/minecraft-operator/operator
      newTag: vX.Y.Z
```

### Configuration

The operator reads flags or the matching environment variables on its
Deployment. The ones worth knowing:

| Environment | Default | What |
|---|---|---|
| `SUPERVISOR_IMAGE` | `ghcr.io/andreabedini/minecraft-operator/supervisor:<version>` | the supervisor copied into server pods |
| `JAVA_IMAGE_TEMPLATE` | `eclipse-temurin:%d-jre` | the server image, `%d` is the Java major |

Run `operator -help` for the rest.

## Supervisor

The supervisor is the in-pod agent. It manages one directory and one
long-running process and knows nothing about Minecraft. Build and run it
locally:

```sh
mise run build-supervisor
printf 'dev-token\n' > /tmp/token
bin/supervisor -data-root /tmp/mc -listen 127.0.0.1:9800 -token-file /tmp/token
```

It speaks gRPC, gRPC-Web and Connect on one port, with server reflection, so
`grpcurl` works without the proto:

```sh
H='authorization: Bearer dev-token'
grpcurl -plaintext -H "$H" 127.0.0.1:9800 list
grpcurl -plaintext -H "$H" 127.0.0.1:9800 supervisor.v1.SupervisorService/Status
grpcurl -plaintext -H "$H" -d '{"spec":{"command":"java","args":["-jar","server.jar","nogui"],"stopCommand":"stop","autostart":true}}' \
  127.0.0.1:9800 supervisor.v1.SupervisorService/SetLaunch
grpcurl -plaintext -H "$H" 127.0.0.1:9800 supervisor.v1.SupervisorService/Start
```

Flags: `-data-root`, `-listen`, `-token-file`, `-readonly-token-file`,
`-download-allow-host` (repeatable), `-console-lines`, `-no-autostart`,
`-no-auth` (development only), `-log-level`.

## Operator

```sh
kubectl apply -k config/default
kubectl apply -f - <<'YAML'
apiVersion: minecraft.bedini.au/v1alpha1
kind: MinecraftInstance
metadata:
  name: craft
  namespace: minecraft
spec:
  version: "26.3"
  flavour:
    fabric: {}
  storage:
    size: 20Gi
    storageClassName: badssd-fs-retain
YAML
kubectl -n minecraft get mci craft -o yaml
```

The operator creates the Secret, PVC, Services and Deployment, waits for the
pod, drives the supervisor through the install (downloads, EULA, properties,
config files, launch spec) and starts the server. `spec.stopped: true` stops
the server without removing the pod.

## Development

Tools are declared in `mise.toml` (buf, protoc-gen-go, protoc-gen-connect-go,
grpcurl, golangci-lint, controller-gen, ko, kubectl).

```sh
mise run generate   # buf generate, controller-gen
mise run test       # go test -race ./...
mise run lint
mise run e2e        # kind end-to-end tests, see docs/testing.md
```

Images are built with [ko](https://ko.build), configured in `.ko.yaml`.
To build and push both to your own registry:

```sh
KO_DOCKER_REPO=ghcr.io/you/minecraft-operator VERSION=dev \
  ko build --base-import-paths --tags dev ./cmd/operator ./cmd/supervisor
```

CI (`.github/workflows/images.yaml`) runs `go vet` and `go test -race`,
then builds both images on pull requests, pushes them on `main` and on
`v*` tags, and publishes a GitHub release with `install.yaml` for each tag.
