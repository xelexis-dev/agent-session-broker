### Kimi Code (macOS·Linux, turn-boundary delivery)

Kimi reads `.mcp.json` at the project root. Use the same format as the Claude example above. **Both server and client must be 0.17.2 or later.** An older server produces `unsupported-server` before any registration attempt. Kimi on Windows is unsupported.

Configure these hooks in Kimi's home at `~/.kimi-code/config.toml` (or `$KIMI_CODE_HOME/config.toml`). Global configuration is not changed automatically. Do not put the broker token in hook commands.

Install the same pinned version as your MCP configuration with `npm install -g @xelexis/x-broker@1.2.1`. Use the absolute path printed by `command -v x-broker` in the hook commands below. The hook's `PATH` must resolve `node`. Keep the global install and MCP versions aligned when upgrading.

```toml
[[hooks]]
event = "SessionStart"
command = '"/absolute/path/x-broker" kimi-hook --auth-dir "/absolute/path/client-state"'
timeout = 5

[[hooks]]
event = "UserPromptSubmit"
command = '"/absolute/path/x-broker" kimi-hook --auth-dir "/absolute/path/client-state"'
timeout = 5

[[hooks]]
event = "Stop"
command = '"/absolute/path/x-broker" kimi-hook --auth-dir "/absolute/path/client-state"'
timeout = 5
```

Use the same path for the MCP client's `--auth-dir`/`--state-dir` and the hook. If both are omitted, they use the same `AGENT_SESSION_BROKER_STATE_DIR` environment variable and default user path. Different paths prevent the hook from finding the socket, producing `kimi-session-unknown`. For a long state path, only the socket moves to a short, private user directory whose ownership is verified. Socket bind failures appear as `kimi-hook-unavailable` in `auth_status`.

Messages arrive at the **next eligible UserPromptSubmit or Stop hook**. Idle, failed or already Stop-continued turns may wait until the next user input. Idle conversations are not awakened. Check `delivery: "turn-boundary"` and `pendingMessages` in `auth_status`. In the public session list, `automaticDeliveryReady: false` describes this limitation; it does not prevent queue acceptance. Activity is `unknown / unsupported`.

Kimi 2.3.1 runs only one Stop continuation per turn. Messages arriving afterward remain until the next user input. If a hook is interrupted by Esc, timeout or another cause after the client hands it a batch, that batch may be lost. The `kimi-hook-handoff` diagnostic records only handoff, not model receipt. Changing conversations blocks the old receiver and shows `reconnect-required`. Reconnect MCP for the new conversation, then explicitly request connection.
