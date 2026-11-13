## Upgrading while connected

`auth_status.serverVersion` can be `unknown` with `serverVersionKnown: false` even while connected: a successful reply stopped reporting version metadata after previously reporting it. Reconnect MCP to refresh the handshake version.

**1.0 change:** spawn tool/issuance removed. Atomic v5 scope cleanup; empty lists refuse startup without changes. Full responses retain nonfunctional `session:spawn` until 2.0 for compatibility. Rollback requires full-backup restore.

Recommended client: 1.2.1 (Node.js 22 or later). Server and client both require peer version 0.17.3 or later and reject legacy/placeholder versions. Update clients on all PCs and reconnect MCP before replacing the server. Attachments require server, sender and recipient clients 0.18.0 or later.

`auth_status.update` appears only when `clientVersion` is below the connected server's `recommendedClientVersion`. Server and npm client versions are separate. Older servers without a recommendation show none. The recommendation is a version whose publication was verified for the server release, not the internet's latest.

For global installs, run `npm install -g @xelexis/x-broker@<recommended-version>`, check `x-broker --version`, then restart/reconnect as below. For npx, change the MCP version pin and prepare it with `npx -y @xelexis/x-broker@<recommended-version> --version`. Updating a global install does not change a separate npx pin. `--version` checks the version; it does not update a conversation connection.

- **Claude:** update the installed/configured version, then run `/mcp` → `x-broker` → `Reconnect` in that conversation. Check both the tool list and actual `clientVersion`.
- **Codex:** keep the original host mode and flags. A TUI opened with `-c` uses an embedded app-server; resume without `-c` uses the shared daemon. Exit and reopen with the same flags; resume exec conversations through exec. Reopening a shared-daemon TUI alone does not guarantee MCP restarts.
- **Codex daemon reload:** `config/mcpServer/reload` is experimental, verified in an isolated app-server. On a shared daemon it can affect all threads, including other projects. Use it only after the user assesses the impact. Neither x-broker nor PM runs it automatically.
- **Windows:** npm launchers 0.17.3 or later use verified dedicated executable copies, allowing `npm install -g` while sessions run. For the first update from an old launcher, stop running asb-client processes. Also stop a session before upgrading if stderr said it fell back to the installed original because the cache was unavailable. Running client versions change on MCP reconnection.
- **Kimi:** upgrade/reconnection is unmeasured. Update the installed/configured version and restart Kimi.

Daemon `restart`/`update` can interrupt every conversation on that daemon. After restart, steer verifies records/queues and holds unconfirmed messages and later delivery. Duplicates remain possible on embedded/Windows transitions and in ordinary hook/queue's short storage window. Choose daemon (immediate delivery, restart deduplication) or embedded (per-conversation environment, tool-boundary delivery) for the project; keep the same mode within a conversation.

Codex fixes the tool list per session; reopen the Codex session to see tools added by a new client. Claude rereads tools with `/mcp` Reconnect.

After reconnecting, check `auth_status.clientVersion`, unchanged registration ID/name, `get_session.automaticDeliveryReady` and resumed actual message receipt. Do not retire registration or attach anew just to upgrade.
