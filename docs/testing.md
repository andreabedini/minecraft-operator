# Testing

Four tiers. The first two run on every `mise run test`; the last needs kind.

| Tier | What | How | Where |
|---|---|---|---|
| 1 Unit | paths, console, properties merge, resolvers, plan | `go test` with httptest fixtures | `internal/*/…_test.go` |
| 2 Component | the supervisor over its real API; the reconciler against a fake API server with a real in-process supervisor, fake publishers, a fake `java` and a fake management server | `go test -race` | `internal/supervisor`, `internal/controller` |
| 3 envtest | CRD validation (CEL rules, defaults), status subresource, manager wiring | not yet written | |
| 4 End-to-end | the operator on a kind cluster with real pods | `mise run e2e` | `test/e2e` |

## End-to-end

`mise run e2e` builds three images with podman, creates a dual-stack kind
cluster from `hack/kind-config.yaml` (rootless podman via
`KIND_EXPERIMENTAL_PROVIDER=podman`), loads the images, applies
`config/e2e`, and runs `go test -tags e2e ./test/e2e/...`. The suite is
written with `sigs.k8s.io/e2e-framework` (one `features.New` per scenario,
`Assess` steps) and Gomega assertions.

The cluster is destroyed at the end. `E2E_KEEP=1` keeps it, and
`E2E_KUBECONFIG=… go test -tags e2e ./test/e2e/...` reuses an existing cluster
without creating, loading or destroying anything.

### The fake server

Nothing in the default run touches the internet. `hack/fakeserver` is a Go
binary installed as `/usr/bin/java` in `Dockerfile.fakeserver`, and the e2e
overlay sets the operator's Java image template to that image. Launched by the
supervisor with the real launch spec, it ignores the JVM arguments, reads
`server.properties`, refuses to start without `eula.txt`, listens on the game
port so the readiness probe passes, serves the management protocol on the
configured loopback port with the configured secret, prints the usual startup
lines, simulates a player joining two seconds after start, and reacts to
`stop`, `save-off`, `save-all flush` and `save-on` on stdin.

The same binary in `publish` mode runs as the `fake-upstream` Deployment and
serves the Mojang manifest and Fabric meta endpoints, with every download
pointing at itself. The overlay points the operator's upstream URL flags at
it and lets supervisors download over plain http.

### Scenarios

1. **instance becomes ready**: resources created, `Ready`, resolved Java
   major, protocol version, the fake player in status and a `PlayerJoined`
   event.
2. **server survives an operator restart**: the operator pod is deleted;
   the server pod keeps its UID, no container restarts, no new `Started`
   event.
3. **supervisor autostarts without the operator**: the operator is scaled to
   zero and the server pod deleted; the replacement pod becomes ready on the
   supervisor's persisted launch spec alone; the operator reattaches after
   scale-up.
4. **deleting the instance keeps the claim**: the Deployment is garbage
   collected, the PVC stays.

Planned: config drift while running, `podOverrides` with a sidecar, adoption
of an existing claim, and an opt-in run against a real Fabric server from
Mojang and FabricMC, which is the only way to exercise the real management
protocol and a world upgrade.

## Homelab

Before the cutover in the design doc: a scratch instance with a fresh world
in its own namespace, then adoption against a copy of the Lodestone
directory, then the live world.
