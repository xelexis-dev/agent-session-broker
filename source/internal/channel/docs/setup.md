### Installation and MCP configuration

Requires Node.js 22 or later and npm. Supports Windows x64, macOS ARM64·x64 and Linux x64. A global install pinned to a published version is the default.

```sh
npm view @xelexis/x-broker version
npm install -g @xelexis/x-broker@1.2.0
x-broker --version
```

These examples pin `1.2.0`. Server-only releases skip npm publication, so server and client versions may differ. For another version, check that it is published and change the install version. `--version` checks the version, not the conversation connection.

Alternatively, use `npx -y @xelexis/x-broker@1.2.0`; an npm process of roughly 100 MB remains for each session. Change `command` below to `npx` and prepend `"-y", "@xelexis/x-broker@1.2.0"` to `args`. Pin a version: `@latest` may delay startup with registry checks and downloads.

On macOS·Linux, a Node global wrapper with execve support replaces itself with the client. **Windows has no execve: a node relay remains with either install method, while a global install removes the npm exec parent process.**

## 3. MCP configuration

If configuration already exists, add or replace only the `x-broker` entry. Replace the server address, `project=brk` and `YOUR_ISSUED_TOKEN` with your values. Do not put files containing tokens in Git or shared folders.

Adding `"--tool-profile", "worker"` to MCP `args` hides seven management and bug-query tools in every harness and rejects their calls with `tool-not-in-profile`. The default is `full`. This does not change server permissions or security boundaries.

**Claude Code — `.mcp.json`**

```json
{
  "mcpServers": {
    "x-broker": {
      "command": "x-broker",
      "args": [
        "--broker-url", "https://broker.example/mcp?project=brk",
        "--token", "YOUR_ISSUED_TOKEN"
      ]
    }
  }
}
```

**Codex — `.codex/config.toml`**

```toml
[mcp_servers.x-broker]
command = "x-broker"
args = [
  "--broker-url", "https://broker.example/mcp?project=brk",
  "--token", "YOUR_ISSUED_TOKEN"
]
```

Codex reads configuration only from trusted projects. Keep the same OS user and authentication state directory. After reconnecting MCP, call `attach_session` only when the user asks to connect this conversation. Existing registrations recover automatically. Check the registered name, clientVersion and serverVersion with `auth_status`. If an approval code appears, approve it in the control panel. Read `xbroker_docs topic=upgrade` for reconnection details or `topic=errors` for connection errors.
See [Connecting to multiple projects](#connecting-to-multiple-projects) for one entry per project. With a custom `CODEX_HOME`, specify the same path in each MCP entry's `env` too.
