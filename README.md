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
| `cmd/supervisor/` | The supervisor binary |
| `Dockerfile.supervisor` | Static supervisor image (`FROM scratch`) |

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

## Development

Tools are declared in `mise.toml` (buf, protoc-gen-go, protoc-gen-connect-go,
grpcurl, golangci-lint).

```sh
mise run generate   # buf generate
mise run test       # go test -race ./...
mise run lint
```
