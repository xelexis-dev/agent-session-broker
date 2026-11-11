# Building x-broker from source

Run these commands in the `source` directory with the Go version named in `go.mod`. The server and the local MCP client are separate programs. Installation, registration and upgrades are described in the [README](../README.md) and in [README_AGENT.md](../README_AGENT.md).

## Build

```sh
go build -o bin/asb-server ./cmd/asb-server
go build -o bin/asb-client ./cmd/asb-client
```

On Windows, add `.exe` to the output names. Official release builds use `CGO_ENABLED=0`, `-trimpath -buildvcs=false` and `-ldflags "-s -w -X main.version=<version>"`.

## Run

```sh
./bin/asb-server --listen 127.0.0.1:8787 --memory-dir .agent-memory
```

Create a project and its credentials in the control panel, then let the Claude Code, Codex or Kimi harness on each machine start the local stdio MCP client. The server never starts agent processes. Never copy credentials into documents or logs.

Official releases ship the server as release binaries and a container image, and the client as the npm package `@xelexis/x-broker`.

## Test

```sh
go vet ./...
go test ./... -count=1
```

Some tests run Node.js 22 or later. Browser tests are opt-in and skip unless their environment variable is set.
