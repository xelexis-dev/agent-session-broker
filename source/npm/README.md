# {{name}}

The MCP client for [Agent Session Broker](https://github.com/xelexis-dev/agent-session-broker-deploy#readme): connect Claude Code, Codex and Kimi conversations so they can message each other.

English | [한국어](https://github.com/xelexis-dev/agent-session-broker-deploy/blob/main/README.ko.md)

Requires Node.js 22 or later and npm. One package contains four clients: Windows x64, macOS arm64 and x64, and Linux x64.

## Install

```sh
npm install -g {{name}}@{{version}}
x-broker --version
```

Pin the published npm version. Server and client versions can differ.

From client 0.17.3, Windows runs a verified copy per version and SHA-256 from `%LOCALAPPDATA%\xelexis\x-broker\client-cache\v1\win32-x64`, so you can upgrade the npm package while sessions started by the new launcher keep running. **When you first upgrade from a 0.17.2 or older launcher, stop every running asb-client once.** A running session picks up the new version when its MCP restarts. If the cache cannot be prepared, the launcher prints one line to stderr and runs the installed original, so stop that session before the next upgrade. A run that creates a new cache, after it starts successfully, removes old copies published more than 7 days earlier (up to 16) and staging older than 1 hour; locked files stay.

## MCP setup

With `@latest`, npx may check the npm registry and download before the MCP process starts, which delays it. Pin the published version as in the examples and run `--version` of the same version first to prepare it. `--version` only prints the version; it does not test a conversation connection. Do not judge whether a conversation is connected from a run outside the harness without arguments.

The default is a pinned global install and the `x-broker` command. The alternative, `npx -y {{name}}@{{version}}`, leaves an npm parent process of about 100 MB per session. To use npx, change `command` below to `npx` and put `"-y", "{{name}}@{{version}}"` before the other `args`.

On macOS and Linux, a Node that supports execve replaces the wrapper with the client. Windows has no execve, so a node relay stays either way, but a global install removes the npm exec parent.

Replace the server address, project name and token with real values. Never put a token in Git or a shared file.

**Claude Code — `.mcp.json`**

```json
{
  "mcpServers": {
    "x-broker": {
      "command": "x-broker",
      "args": ["--broker-url", "https://broker.example/mcp?project=brk", "--token", "<project token>"]
    }
  }
}
```

**Codex — `.codex/config.toml`**

```toml
[mcp_servers.x-broker]
command = "x-broker"
args = ["--broker-url", "https://broker.example/mcp?project=brk", "--token", "<project token>"]
```

Codex reads the config of trusted projects only. When you change an existing config, keep the same OS user and auth state directory.

Restart the MCP connection and ask the agent to connect this conversation to x-broker. A new conversation registers only when you ask it to connect. If an approval code appears, approve it in the control panel. A registration token without approval registers at once. A conversation that is already registered reconnects automatically.

Upgrading only the server does not change the tool list of a client that is already running. To use new tools, also raise the package version in the install or the MCP config and reconnect each conversation.

See the [agent guide](https://github.com/xelexis-dev/agent-session-broker-deploy/blob/main/README_AGENT.md) (Korean) for detailed MCP setup and the [installation guide](https://github.com/xelexis-dev/agent-session-broker-deploy#readme) for the server.

To connect one conversation to several projects, see [multiple projects](https://github.com/xelexis-dev/agent-session-broker-deploy/blob/main/README_AGENT.md#여러-프로젝트에-연결하기) (Korean). Give each project its own MCP entry, and set the Codex hooks and Kimi state paths per entry.

## Upgrade

For a global install, run `npm install -g {{name}}@<recommended version>`, check `x-broker --version`, then restart the session or reconnect MCP. With npx, change the pinned version in the config, prepare it with `npx -y {{name}}@<recommended version> --version`, then reconnect. Updating the global install does not change a separate npx pin. A running client keeps its version until it reconnects. For the full procedure see `xbroker_docs topic=upgrade`, and for errors `topic=errors`.

## Message attachments

Pass a path relative to this conversation's CWD in the optional `attachment` argument of send_message. One file of at most 100 MiB; it needs the administrator switch and a Claude or Codex recipient that supports ACK. Links, hard links, outside paths and credential patterns are refused. An older server rejects only the attachment as unsupported; do not resend automatically after outcome-unknown.

Received files are stored in the OS cache under `agent-session-broker/inbox/<sessionKey>/`. The inbox has no size limit and does not delete completed files. The model receives only the path, size and SHA-256, or an error. Attachments are untrusted data and are never opened automatically. The server deletes its temporary file 30 minutes after upload, or on ACK, when the switch is turned off, or on restart; files already received cannot be recalled. The path check cannot detect a secret that was copied or renamed.

Third-party license texts and notices are included in `THIRD_PARTY_NOTICES`. The software is MIT licensed; see LICENSE.
