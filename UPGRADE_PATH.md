English | [한국어](UPGRADE_PATH.ko.md)

# Upgrade paths

## Current distribution

New GitHub Releases provide four server executables plus LICENSE and THIRD_PARTY_NOTICES (six assets). On Windows, macOS and Linux, check the published client version with `npm view @xelexis/x-broker version`, prepare it with `npx -y @xelexis/x-broker@<version> --version`, then reconnect MCP. Keep existing client state and credentials. Server-only releases may skip npm publication. Existing Releases and client downloads through 0.14.0 remain available; executable instructions below describe the distribution method used at the time.

This guide covers updates and data migration **through 1.1.0**. See the [installation guide](https://github.com/xelexis-dev/agent-session-broker#readme) for replacing executables and [public releases](https://github.com/xelexis-dev/agent-session-broker/releases) for files and changes. Files for 1.0.2 and earlier remain in the [previous distribution repository](https://github.com/xelexis-dev/agent-session-broker-deploy/releases). Docker images older than 1.0.3 are no longer available; use server executables when upgrading through or restoring those versions.

0.20.0 published only an npm client, with no server Release. Release candidates containing both server and client start at 0.20.1.

## 1.0.4 → 1.1.0

The control panel is available in English and Korean. Each account chooses its language in My profile. English is the default, so existing accounts start in English. Pages before sign-in follow the language last used in the browser (the `asb_lang` cookie), and English without it. Agent-facing text is English: MCP tool descriptions and results, Management API errors, client diagnostics and command-line output. Machine codes are unchanged. Clients 0.17.3 and later keep working with the 1.1.0 server; client 1.1.0 carries the English agent-facing text. The server adds one language setting per account and does not change the format of existing data. Upgrade the client to `@xelexis/x-broker@1.1.0`, stop the server and back up all data, replace the server with 1.1.0 (Docker: `ghcr.io/xelexis-dev/agent-session-broker:1.1.0`), then reconnect each conversation. Rollback requires restoring the complete backup together with the 1.0.4 server.

## 1.0.3 → 1.0.4

Behavior and data formats are unchanged from 1.0.3. From 1.0.4 the Docker image is named `ghcr.io/xelexis-dev/agent-session-broker`. The old name, `ghcr.io/xelexis-dev/agent-session-broker-deploy`, is retired and will be deleted. 1.0.3 is also available as `ghcr.io/xelexis-dev/agent-session-broker:1.0.3`; older images are no longer provided. Earlier server executables remain in Releases in the [previous distribution repository](https://github.com/xelexis-dev/agent-session-broker-deploy/releases). For Docker Compose, change `image:` in `compose.yaml` to `ghcr.io/xelexis-dev/agent-session-broker:1.0.4`. If `BROKER_IMAGE` specifies the image, change that value too. The 1.0.3 image has the same digest under the new name, so you may change the name first and upgrade the version later. Executable deployments have no image change. Client 1.0.4 changes only the version, with unchanged behavior. Upgrade the client to `@xelexis/x-broker@1.0.4`, stop the server and back up all data, replace the server with 1.0.4, then reconnect each conversation. Rollback requires restoring the complete backup together with the 1.0.3 server.

## 1.0.2 → 1.0.3

Behavior and data formats are unchanged from 1.0.2. Server executable Releases, npm client provenance and source now come from [public repository](https://github.com/xelexis-dev/agent-session-broker). The npm repository metadata changed, so client 1.0.3 is also published. The Docker image name (`ghcr.io/xelexis-dev/agent-session-broker-deploy`) is unchanged. Upgrade the client to `@xelexis/x-broker@1.0.3`, stop the server and back up all data, replace the server with 1.0.3, then reconnect each conversation. Files for 1.0.2 and earlier remain available from the previous distribution repository. Rollback requires restoring the complete backup together with the 1.0.2 server.

## 1.0.1 → 1.0.2

Behavior and data formats are unchanged from 1.0.1. Rebuilding with the Go 1.27.4 security release also changes the client executables, so npm client 1.0.2 is published. Upgrade the client to `@xelexis/x-broker@1.0.2`, stop the server and back up all data, replace the server with 1.0.2, then reconnect each conversation. Rollback requires restoring the complete backup together with the 1.0.1 server.

## 1.0.0 → 1.0.1

Behavior and data formats are unchanged from 1.0.0. The Go module path changed for the public source release, changing the client executables too, so npm client 1.0.1 is published. Upgrade the client to `@xelexis/x-broker@1.0.1`, stop the server and back up all data, replace the server with 1.0.1, then reconnect each conversation. Rollback requires restoring the complete backup together with the 1.0.0 server.

## 0.20.2 → 1.0.0

This is the first stable release. APIs, data formats and client behavior are unchanged from 0.20.2. Upgrade the client to `@xelexis/x-broker@1.0.0`, stop the server and back up all data, replace the server with 1.0.0, then reconnect each conversation. Rollback requires restoring the complete backup together with the 0.20.2 server.

## 0.20.1 → 0.20.2

This release replaces control panel icons with unmodified Lucide icons. APIs, data formats and client behavior are unchanged. Upgrade the client to `@xelexis/x-broker@0.20.2`, stop the server and back up all data, replace the server with 0.20.2, then reconnect each conversation. Rollback supports only the complete backup restore described below.

## 0.19.x → 1.0

Follow these steps when upgrading from 0.19.x or earlier to 1.0. The 0.20.x release candidates already use the 1.0 policy; for those, replace the client and server as described under `0.20.2 → 1.0.0` and the subsequent 1.0.x sections.

**Clients older than 0.17.3 can no longer connect.** This includes PCs belonging to other projects, so upgrade clients first.

1. **Find remaining old clients:** ask the people responsible for each project to check client versions on each PC and update anything older than 0.17.3. The version summary is removed in 1.0, so check powered-off PCs separately too.
2. **Upgrade clients first:** on every PC, run `npm install -g @xelexis/x-broker@1.0.3` (or update the pinned version in the MCP config for npx), then reconnect each conversation. The 1.0 client works with servers 0.17.3 and later, including the current 0.19.x server, so clients may be updated first. Follow [upgrading while connected](https://github.com/xelexis-dev/agent-session-broker/blob/main/README_AGENT.md#upgrading-while-connected) to reconnect.
3. **Make a complete backup:** stop the server and back up the entire data directory. Record the file list and hashes, and restrict access to the backup: it contains token hashes and configuration. Drain pending messages first; the server's in-memory queue is not backed up.
4. **Replace the server:** start the new server with the same data path. For Compose, change the image version and run `docker compose pull` and `docker compose up -d`.
   - A server that has completed initial setup does not need a setup code.
5. **Check old-client refusal:** every tool call from clients older than 0.17.3 is refused. See [version compatibility](https://github.com/xelexis-dev/agent-session-broker/blob/main/README_AGENT.md#version-compatibility) for the guidance shown.
6. **Check connections:** in each conversation's `auth_status`, check the versions, `compatible`, and unchanged registration ID and name. Verify actual peer message exchange.

**Rollback** supports only a complete backup restore.

1. Stop the new server. Keep the problematic data directory separately rather than deleting it.
2. Restore the complete backup from step 3 into a **new, empty directory**, and verify its file list and hashes against the record.
3. Start the server version that made the backup (0.19.x). Do not start a 0.19.x server directly on data from the new version.
4. Before reopening external connections, **revoke again every token, credential and user API token revoked after the backup**. Tokens issued, memory changes and permission changes made after the backup are not restored.
5. Clients do not need to be downgraded: the 1.0 client also works with a 0.19.x server. The restored 0.19.x server will accept clients older than 0.17.3 again.

### Stored data and tools changed in 1.0

Backups must include all instance data, including control, registration, bugreport and memory, plus client state. Message queues exist only in memory.

- `spawn_session` is removed. Cached calls return an unknown-tool error; reconnect MCP. Start harnesses directly on each PC.
- At startup, one atomic save removes only `session:spawn` from every token, grant and device-approval scope set. The control version remains 5; other scopes, admission modes, expiry, revocation state and audit history are preserved. Restarting does not save the same cleanup again.
- If this would leave an invalid empty scope list, startup **refuses without writing anything**. Normal issuance and editing in 0.17.3–0.19.2 retain `session:register`. Do not invent scopes or replace them with an empty array; restore a complete backup made while stopped and investigate.
- Reading `session:spawn` in historical audit entries and old-server responses remains supported, but new issuance, editing and delegation are refused.
- Full-permission responses display `session:spawn` through 2.0 for old-client compatibility. It has no function and is removed from stored values, new issuance, editors and the scope catalog. It is not added to restricted-permission responses.

## 0.19.2 → 0.19.3

0.19.3 prepares for 1.0. Minimum compatible versions and storage formats are unchanged.

1. Back up all instance data and replace the server with 0.19.3 using the same data path. 0.19.3 also publishes a new npm client. Existing clients (0.12 and later) continue to work with the 0.19.3 server.
2. As an administrator, open **Instance settings → Client version status**. It shows live registrations by client version across all projects, counts below 0.17.3 and of unconfirmed versions, compatibility refusals over 24 hours, and the last observation time (UTC). Registration names, IDs and tokens are not shown. This status is kept only in memory and starts over after a server restart. It reflects current leases and cannot establish that unobserved or stopped clients are safe.
3. Observe the status for at least 48 hours before upgrading to 1.0, and notify projects with clients below 0.17.3.
4. Already configured servers are unaffected. An unconfigured server requires the code in `control/setup-code` under its data directory to create the first administrator. Logs show the file location, not the code. Restarting changes the code; successful setup deletes it. Rolling back an unconfigured server restores the old setup behavior without a code, so prevent external exposure and configure it through loopback or an SSH tunnel. Pre-setup backups may also contain the code; restrict access to them.

For rollback, stop the server and restore both the complete pre-upgrade backup and the 0.19.2 executable (or image).

## 0.19.1 → 0.19.2

1. Back up all instance data and replace the server with 0.19.2. There is no required storage migration or minimum-version change.
2. If MCP reconnection failed after Claude Code updated itself while running on Windows, run `npm install -g @xelexis/x-broker@0.19.2` and reconnect MCP. Sessions started after the update were unaffected. Before installing the fix, stopping and resuming the session is a workaround.
3. If parent identification fails, inspect `auth_status.parentDiagnostic` for the failing step, error number and allowlisted program names. Paths, arguments and tokens are not shown.

## 0.19.0 → 0.19.1

1. Back up all instance data, then replace the server with 0.19.1 using the same data path. No required storage migration is introduced; the release includes a tablet sidebar fix for the control panel.
2. To see broker messages in the VS Code Claude extension, confirm npm publication, manually run `npm install -g @xelexis/x-broker@0.19.1`, and reconnect each MCP instance. New messages show the sender and the beginning of the body in a collapsed "Sender not recorded" peer row. Delivery formats in the terminal CLI and other hosts are unchanged.
3. The minimum server/client version of 0.12.0 and legacy allowance remain unchanged. Old messages and the plain fallback for large messages may remain hidden in VS Code.

## 0.18.1 → 0.19.0

1. During an approved maintenance window, drain undelivered messages as far as possible. Stop the server and back up all instance data, including control, registration and bugreport. In-memory queues are not backed up.
2. Replace the server with 0.19.0 using the same data path. The management API defaults to OFF; existing MCP use does not need an API token. Enable it in administrator settings only if needed.
3. Check HTTPS and the existing trusted-proxy configuration. When Allow HTTP connections is OFF, plain-HTTP API requests are refused. Allowing plain HTTP accepts the risk of exposing passwords and tokens.
4. Prepare management API automation with a separate regular user and minimum project memberships and scopes. Issue your own token with your current password on the profile API-token screen, or use the login API. An administrator write token has Owner permissions in every project; device approval needs the separate approve scope. Never put raw credentials in logs, Git or command arguments.
5. Confirm npm publication, manually update clients that need it to `@xelexis/x-broker@0.19.0`, and reconnect each MCP instance. The management REST API is a server feature, so updating all existing MCP users is not required. The minimum server/client version of 0.12.0 and legacy allowance remain unchanged. Check versions and registration IDs in auth_status, and actual peer receipt.

For rollback, stop the server and restore the complete backup together with the previous executable. Changes after the backup and in-memory queues are not restored. User API tokens may become active again after re-upgrading even if dormant in 0.18.x. For credential exposure, revoke them before downgrading; if already downgraded, change the affected user's password. Switching the API OFF does not revoke tokens.

## 0.17.3 → 0.18.0

File attachments require the server and both sending and receiving clients to be updated. Prepare HTTP policy and backups first, then follow this order.

1. Determine whether the current server uses direct HTTP or sits behind a TLS proxy. When HTTP is disallowed, plaintext connections are refused even from localhost. For TLS proxies, prepare `--trusted-proxy <proxy CIDR>` and overwrite `X-Forwarded-Proto` with the single original scheme. To keep using direct HTTP, explicitly enable HTTP in administrator policy. Do not trust an unknown proxy.
2. During an approved maintenance window, drain undelivered messages as far as possible. Stop the server and back up all instance data, including control, registration, bugreport and deletion history, plus client state. ACK queues exist only in memory.
3. Replace the server with 0.18.0 using the same data path. There is no new mandatory storage migration. Registrations whose LastSeen is at least 30 days old and whose response deadline has expired may be detached. Suspended registrations are excluded; name and credential cleanup follows the existing verified policy.
4. After npm publication of 0.18.0 is verified, update clients to `@xelexis/x-broker@0.18.0`. Reconnect Claude through `/mcp`; reopen Codex sessions with their original launch mode and flags. Resuming a shared daemon does not prove that a new MCP process started. If the Windows launcher reported fallback to the original installation, stop that client before updating.
5. Check clientVersion/serverVersion in `auth_status`, the existing name and registration ID, and actual peer message receipt. The minimum compatible server/client version remains 0.12.0, but client fixes require new executables. For rollback, stop the server and restore the complete pre-upgrade backup together with the previous executable. Later changes and in-memory queues are not restored.

To use attachments, verify that the server and both clients are at least 0.18.0, then enable the administrator switch (OFF by default). Existing messages work without attachments enabled. Server `attachments-v1` data is temporary and is cleared on restart. Completed inbox files remain in the user cache without automatic deletion; clean them up separately. See the attachment section in the [release changes](https://github.com/xelexis-dev/agent-session-broker-deploy/releases/tag/0.18.0) for size limits, credential-file refusal, disk protection and Windows junction restrictions.

## 0.17.2 → 0.17.3

1. Stop the server and back up all instance data, including control, registration and bugreport, plus client state. ACK queues exist only in memory; drain them as far as possible before restarting.
2. Start server 0.17.3 using the same data path. It retains controlVersion 5 from 0.17.2 and adds no mandatory migration.
3. Upgrade the npm client to `@xelexis/x-broker@0.17.3` and confirm with that version's `--version`. On Windows, **stop all running asb-client processes once when first moving from the old launcher**. The new launcher runs a verified copy outside the installation directory, but connections that report cache fallback must be stopped before the next npm update.
4. Reconnect Claude through `/mcp`; reopen Codex with its original launch mode and flags. Check each Codex session's tool list and clientVersion; resuming a shared daemon does not prove a new MCP process started. Verify the same registration ID and name, and actual peer message receipt. Kimi upgrades have not been measured.

The new retire and bug-report tools require 0.17.3 on both sides. Updating or reconnecting the server alone does not add tools to an old client; a new client calling an old server receives unsupported guidance. When both sides negotiate message ACKs, redelivery continues until host handoff; this is not confirmation that the model read the message. When downgrading to an old client with outstanding unacknowledged messages, `storage` retries protect them for a fixed maximum of 10 minutes from the first incident, then delivery returns to legacy mode. Server restart, retire, administrative detachment, suspension and identity-invalid discard messages.

Server ACKs are memory-only, so there is no persistent queue format change. Bug reports add resolution, notes and recent history to open/closed status. 0.17.2 can read them, but writes by an old server lose extension fields and may refuse status changes for records above 32 KiB. Lossless rollback is not guaranteed. For rollback, stop the server and **restore the complete pre-upgrade backup together with the previous executable**. Changes after the backup and pending in-memory messages are not restored.

Diagnostics v2 are written to separate files; v1 is read-only. Downgrading to 0.17.2 hides v2 diagnostics, which appear alongside v1 after re-upgrading. Include deletion history, `deletions.db`, in the complete backup. Running an old version alone does not restore revoked credentials or deleted reports. If an old server saves a registration again, its reapproval capability may be lost; after re-upgrading it is excluded from automatic revocation until a new holder advertises that capability.

Choose daemon mode (immediate delivery and restart duplicate protection) or embedded mode (per-conversation environment and delivery at tool boundaries) for each project, and keep the mode consistent within a conversation. The new launcher can resume daemon/embedded conversations without an initial prompt, but check the running MCP version separately. Restarting or updating the daemon also disconnects other conversations. Steer records an attempt without the body before sending, then checks conversation history and the queue after restart; unconfirmed messages and later deliveries are held. Duplicate delivery remains possible during embedded/Windows transitions and the small persistence window in existing hooks and queues. Windows has no steer support. Restricted-account Windows testing found no product defects; the final rerun after test/environment fixes was still pending. Linux/macOS amd64 hardware validation is separate.

## 0.17.1 → 0.17.2

1. Stop the server and back up the **entire instance data directory**, including control, registration and bugreport, together with existing client state.
2. Replace the server first with 0.17.2 and start it with the same data path. The first startup assigns a generation ID to existing projects once and raises `controlVersion` to 5. Generation IDs survive restarts and backup restoration.
3. Change the npm version in each PC's MCP configuration to `@xelexis/x-broker@0.17.2`. Prepare it with `npx -y @xelexis/x-broker@0.17.2 --version`.
4. For each conversation, reconnect Claude through `/mcp` or restart it. Restart the Codex application/process. Reconnect Kimi MCP too. Check both versions in `auth_status`, the existing registration, and actual message receipt.

Server 0.17.2 works with client 0.17.1. Project-history isolation and the control panel need only a server update; project-ended guidance, recipient registration expectations and receiver-process protection require 0.17.2 on both sides. Old-client tool inputs and delivery fields are unchanged, and registration races between old clients are not protected. Old clients must restart after receive permission is re-enabled.

Client 0.17.2 with server 0.17.1 cannot determine project termination or protect receiver processes. It refuses `expectRegistrationId` with `unsupported` and never resends without the expectation. Kimi requires both server and client 0.17.2 or later; older servers return `unsupported-server` guidance.

Add `turn_id` to Codex hooks **only after upgrading the client to 0.17.2 or later**; 0.17.1 rejects unknown keys. For Kimi, configure `kimi-hook` for `SessionStart`, `UserPromptSubmit` and `Stop`, using the same state path as MCP. It does not wake idle conversations; messages arrive at the next prompt or turn end. Follow the examples in [README_AGENT.md](https://github.com/xelexis-dev/agent-session-broker/blob/main/README_AGENT.md).

Data-write compatibility between versions is not guaranteed. For rollback, stop the server and **restore the complete pre-upgrade data backup together with the previous executable**. Do not restore only `control.db`. Changes after the backup are not included in a complete restore. Undelivered messages do not survive server restart, and failed messages are not resent automatically.

## 0.17.0 → 0.17.1

1. Stop the server and back up its entire data directory. Preserve existing client state too.
2. Replace the server first with 0.17.1 and start it using the same data path. Running sessions automatically reconnect with the same registration about one second after the server returns; some environments take longer.
3. Change the npm version in each PC's MCP configuration to `@xelexis/x-broker@0.17.1`. Prepare it with `npx -y @xelexis/x-broker@0.17.1 --version`.
4. For each conversation, reconnect Claude through `/mcp` or restart it. Restart the Codex application/process. Check both versions in `auth_status`, the existing registration, and actual message receipt.

Server 0.17.1 retains connections from client 0.17.0. The tool list of a running old client is unchanged, so `suspend_session`, `unsuspend_session` and `report_bug` also need client 0.17.1. Existing `stop_session` calls that disconnect other sessions work within their permission boundaries with a server-only update.

Client 0.17.1 can also connect to server 0.17.0, but `/clear` registration handover, management of other sessions, reading current server permissions and `report_bug` are unavailable. Undelivered messages do not survive server restart, and failed messages are not resent automatically.

For rollback, stop the server and **restore the complete pre-upgrade backup together with the previous executable**.

## 0.9.x → 0.17.0 summary

Before replacing any version, stop the server and back up the complete data directory. Keep the existing data path and client state. The table lists changes documented in the [release notes](https://github.com/xelexis-dev/agent-session-broker-deploy/releases); it does not mean that every listed version must be installed in sequence.

| Version | Data and configuration checks |
| --- | --- |
| 0.9.0 | Saves nickname changes and recovery state. Keeps existing registration IDs and tokens. Follow the 0.9.0 instructions below. |
| 0.9.2 | Introduces npm distribution. Keeps data formats and credentials. 0.9.1 was not publicly released. |
| 0.10.0–0.14.0 | Improves the control panel, connections and ownership handling. Keeps existing data and accounts; no separate manual data conversion is prescribed. |
| 0.15.0 | **Upgrades the management DB format to v4.** Stop the server, back up the complete data directory and start with the same path. Restore the old backup too when downgrading. Client distribution moves entirely to npm. |
| 0.16.0 | Keeps management DB v4. Upgrades from 0.14.0 or earlier must follow the 0.15.0 backup and migration conditions. |
| 0.17.0 | Keeps management DB v4 and standardizes storage on SQLite. Initial JSON import and upgrades from existing 0.15/0.16 SQLite remain supported. Do not manually edit JSON or delete the DB. |

If you issued unlimited tokens in 0.17.0, or used limits above 64 registrations or 90 days, restore the complete pre-upgrade backup when rolling back to 0.16.0 or earlier. The 0.17 server and client require the other side to be at least 0.12.0. Update 0.9.x–0.11.x clients when replacing the server.

## Updating to 0.9.0

**Upgrade directly from 0.8.3 to 0.9.0.** The new `rename_session` tool lets a connected conversation rename its own nickname. Stop the server, back up the entire data directory and replace the server first. Then replace each PC's client and reconnect MCP. Nickname changes require the new version on both server and client.

Keep existing project tokens, session credentials, registrations and client state directories. The old nickname is not retained as an alias after a rename; send messages to the new name. The registration ID and conversation do not change, and the new name survives reconnects and server restarts. Rename state is saved to recover from storage failures during the change. To downgrade, stop the server and restore both the complete pre-update backup and the previous executable. If upgrading from 0.8.2 or earlier, also apply the relevant instructions below.

## Updating to 0.8.3

**Upgrade directly from 0.8.2 to 0.8.3.** This fixes duplicate token issuance when refreshing the project-token completion page. Keep the data directory and replace the server executable or container; data formats and the connection protocol are unchanged. Close completion pages opened before the update and return to project settings. Already issued tokens remain valid; inspect and revoke unwanted duplicates in the list. Replacing the client is not required for this fix. For earlier starting versions, also apply their backup and migration instructions below.

## Updating to 0.8.2

**Upgrade directly from 0.8.1 to 0.8.2.** This fixes an ordering issue that allowed clients to connect before saving approved credentials to disk. Keep existing tokens, registrations and state directories; replace the client executable and reconnect MCP. The patch does not change server storage formats or the connection protocol. If upgrading directly from 0.7.0, also follow the 0.8.1 data-backup and HTTP-policy migration instructions below.

## Updating to 0.8.1

**Upgrade directly from 0.7.0 to 0.8.1.** 0.8.0 was not publicly released and is not a required intermediate version. Stop the server, back up the entire data directory (including the DB and any journals), then replace the server. Keep existing project tokens, session credentials and registration state; 0.7.0 clients can still connect. If you replace a client, reconnect MCP to use the new executable.

- Allow HTTP connections now applies to every project under **User menu → Admin panel → Instance settings**. Migration enables it only if at least one project is not being deleted and every such project previously allowed HTTP. Mixed permissions or no eligible projects result in HTTP being disallowed. Administrators using HTTP should check the policy after updating. Saving it makes all project connections recheck policy.
- Click a project row to view connected sessions, and use the gear at the top to open settings. Project-token last-use dates, registration history and status appear in settings. Existing nickname-bound session tokens are hidden from the screen, but their issued credentials continue to authenticate.
- Open the session-approval screen directly at the `/auth/code` URL supplied by the client. Approval still assigns a nickname.
- Historical project-token usage is supplemented from credentials remaining on the server. Credentials already expired and deleted cannot be counted.
- After updating, check `/healthz`, login, project/session/token lists, HTTP policy and existing session reconnection. An older server may not read new stored fields; **to downgrade, stop the server and restore the complete pre-update backup together with that version's executable**.

## Updating to 0.7.0

**Upgrade directly from 0.6.0 to 0.7.0.** Back up all server data and client state, replace the server first and clients next, then reconnect MCP. Keep existing project tokens, session credentials and registration state.

- Per-project HTTP permission defaults to OFF. A system administrator enables **Project settings → Allow HTTP connections** only for projects that need it. Regular project Owners, Developers and Viewers cannot change this policy.
- HTTP connections with project tokens require server and client 0.7.0 or later. Client 0.6.0 still rejects HTTP URLs; client 0.7.0 does not send credentials over HTTP to an older server without the policy API.
- HTTPS and existing nickname-bound session-token connections still work. Allowing HTTP is an exception for trusted networks; it neither supplies TLS nor establishes the server's authenticity.
- The new policy is persisted across server restarts. Older servers may not understand the new fields; restore the complete pre-update backup together with the matching executable when downgrading.
- Disabling HTTP reconnects the project's active connections. It does not revoke credentials; HTTP clients check policy again.

## Updating to 0.6.0

**Upgrade directly from 0.5.1 to 0.6.0; no intermediate version is required.** Keep existing server data, client state directories, session tokens and MCP settings. Make a complete backup as described under **Before updating**, replace the **server first**, then each PC's client, and restart MCP connections.

- The management state (`control/control.db`) storage format changes to Version 3. Once project tokens or session approval are used, state is saved in that format, which servers 0.5.x and earlier cannot open. Restore both the complete pre-update backup and its matching executable to roll back.
- Existing nickname-bound session tokens and their registrations continue to work. New **project tokens** (`asb_p_`) can be issued in the desired project and shared across multiple conversations' MCP settings. Approve each conversation with its code and nickname under **Session approval** (`/auth/code`) in the management panel.
- Project-token approval requires an **HTTPS** client `--broker-url`. The server still listens over HTTP, so prepare a TLS-terminating reverse proxy before switching to project tokens. Existing HTTP URLs with session-token connections remain supported.
- Client `--state-dir` remains available and is equivalent to `--auth-dir`. Approved conversation credentials are saved per conversation under `sessions/` in the same directory. A conversation registered with a session token at the same server URL (`https://host`) and project keeps its registration when switching to a project token. Changing HTTP to HTTPS makes it a different server identity; disconnect the old registration in the management panel, then approve again.
- A client at 0.5.1 or earlier fails authentication and does not start with a project token. Client 0.6.0 connecting to a 0.5.x server with a project token stops with a server-upgrade requirement.
- After updating, check `/healthz`, administrator login, project/token lists, automatic reconnection of existing sessions and actual replies. For projects adopting project tokens, test one conversation through token issuance → client connection → code approval → appearance in the session list.

Normal supported data from 0.4.0 and 0.2.0–0.3.3 can upgrade directly by using 0.6.0 as the target in the 0.5.0 instructions below. Existing unauthenticated users **must follow the 0.4.0 token-only transition**.

## Updating to 0.5.1

**Upgrade directly from 0.5.0 to 0.5.1; no intermediate version or DB migration is required.** Keep existing server data, client state, tokens and MCP settings. Replace server/client executables and reconnect MCP.

The old `register` prompt and embedded skills/resources are no longer provided. Normal connections register automatically. For explicit recovery, ask `Register the current session with x-broker`. The recovery tool `attach_session` remains available.

Normal supported data from 0.4.0 and 0.2.0–0.3.3 can upgrade directly by using 0.5.1 as the target in the 0.5.0 instructions below. Existing unauthenticated users **must follow the 0.4.0 token-only transition**. Installing 0.5.0 first is unnecessary. Rollback still requires a complete backup restore.

## Updating to 0.5.0

**Upgrade directly from 0.4.0 to 0.5.0; no intermediate version is required.** Keep all server data and client state directories, and replace server and client together. Existing tokens do not need to be reissued.

Normal supported data from 0.2.0–0.3.3 can also upgrade directly, but **must follow the 0.4.0 token-only transition** below. Use 0.5.0 in place of the table's 0.4.0 installation step. Existing support limits still apply to 0.1.x and unknown development, modified or damaged data.

Suspension state and information about suspended receiving connections are added to the registration DB. Clients 0.4.0 and earlier do not guarantee automatic recovery after resumption; replace them with 0.5.0 or later and reconnect MCP. Older servers do not understand suspension policy. **Do not run an older server directly on new-version data; restore the complete pre-update backup together with its matching executable.**

## Required version sequence

**Normal supported data from 0.2.0 through 0.3.3 can upgrade directly to 0.4.0, with no required intermediate version.** However, 0.4.0 switches to token-only authentication, so existing unauthenticated clients must change their configuration.

| Current version / data | Path to 0.4.0 | Checks |
| --- | --- | --- |
| New installation | Install 0.4.0, complete initial setup, create projects and issue tokens | Every client needs a token |
| Normal JSON data from 0.2.0 or 0.2.1 | Replace directly with 0.4.0 and use the existing data directory | Apply JSON → SQLite migration and token transition below |
| Normal SQLite data from 0.3.0–0.3.3 | Replace directly with 0.4.0 and use the existing data directory | No DB engine change; existing open projects are persistently converted to token mode |
| 0.1.x, unknown development versions, modified/damaged data, or versions newer than 0.4.0 | Follow the support limits and older-installation checks below | Migration equivalent to normal supported data is not guaranteed |

## 0.4.0 token-only transition

1. Back up all server data and client state/configuration as described under **Before updating**. Replace server and client with the same version.
2. Start the server using the existing data directory. Previously unauthenticated projects become token-authenticated; accounts, members, registrations, tokens and shared memory are preserved. The unauthenticated-access setting is removed and cannot be restored through the old setting.
3. Existing token-authenticated users keep valid tokens bound to the same conversation and their local registration state. Unauthenticated users must **copy the existing registration's nickname exactly** from the session list, issue a token per conversation using that name, and add `--token "token value"` to MCP settings. `--token-file` is unsupported.
4. **Binding an existing unauthenticated conversation's stored registration to a new token requires one explicit registration.** The token nickname must match the existing registration. A differently named token cannot bind or rename it. Keep the same PC, conversation and client state, then ask `Register the current session with x-broker`. Automatic resume alone cannot bind the new token because it is not yet attached to the registration. Do not delete local state to resolve this.
5. New conversations register automatically after their actual identity is confirmed. If Codex supplies no conversation ID at startup, automatic registration occurs at the first ordinary tool call. Within 30 minutes of token issuance, check that the management panel no longer shows registration pending.
6. Explicitly detached or deleted registrations are not revived automatically. Check the existing nickname, token owner, preserved registration/memory and actual session replies. Claude's old channel mode may require separate channel activation after registration.

Unauthenticated-access instructions in the 0.3.x sections below describe **behavior at that time**. For a 0.4.0 target, also apply the token transition above.

### Direct migration through 0.3.2

The earlier paths and support limits below remain valid. For a 0.4.0 target, skip the 0.3.2 installation step and use the path above.

**No intermediate version is required to upgrade from 0.2.0, 0.2.1, 0.3.0 or 0.3.1 to 0.3.2.** The JSON → SQLite conversion introduced in 0.3.0 remains in 0.3.2 and directly converts the earlier administrator, project and token formats. You do not need to install `0.2.0 → 0.2.1 → 0.3.0 → 0.3.1 → 0.3.2` in sequence.

| Current version / data | Path to 0.3.2 | Intermediate versions and verification |
| --- | --- | --- |
| New installation, no data to preserve | Install 0.3.2 and complete initial setup | No intermediate version |
| Normal JSON management state and persistent registrations from 0.2.0 or 0.2.1 | `0.2.0 → 0.3.2` or `0.2.1 → 0.3.2` | Direct migration supported; verified by conversion code and migration tests reading management state v1 and registration records v1 |
| Normal SQLite data from 0.3.0 | Replace with `0.3.0 → 0.3.2` and keep the data directory | Uses existing SQLite without repeating JSON migration |
| Normal SQLite data from 0.3.1 or 0.3.2 | Keep the data directory for `0.3.1 → 0.3.2` or a 0.3.2 reinstall | No new migration; already migrated JSON is not imported again |
| Data from 0.1.x combined executables, local mode or earlier remote mode | Start with the older-installation checks below | Complete migration of every 0.1.x layout is unverified; no basis for declaring a particular intermediate version necessary or sufficient |
| Unknown development versions, manually modified/damaged data, or versions newer than 0.3.2 | Preserve the original and consult the relevant version's instructions | Outside this guide's supported paths |

Direct support in this table is based on code and tests for valid data formats, not execution of every historical installation on every OS. No private distribution file older than 0.2.0 is required as an intermediate step. Public distribution starts at 0.2.1.

## Before updating

1. Check `--version` on the server and each PC's client. Record executable locations, MCP settings and the server's actual data directory.
2. Stop client MCP connections and the server. Never let old and new servers open the same data directory simultaneously.
3. With the server fully stopped, back up the **entire data directory** and configuration to a separate location. For a native server, use the actual `--memory-dir` path; for Docker, use the host directory or existing volume mounted at container `/data`. Preserve `control`, `projects`, memory documents and DB-related files together. Keep existing local client state too.
4. Download the target server/client versions and compare their SHA-256 values with the Release body. For Docker, pinning the target version tag instead of `latest` makes the replacement easier to verify.

## 0.3.2 → 0.3.3

No intermediate version, DB migration or existing MCP configuration change is required. Replace server and client with 0.3.3, retaining existing data and client registration state directories. Continue using `--token "token value"` and unauthenticated connections as before.

Check saved notifications in the management panel, the add-user modal, the profile's project-role list, user-ID autocomplete in the add-member modal and the separate delete button. Existing accounts, projects, member roles and tokens are unchanged. Also check login, the project list and actual session replies.

## 0.3.1 → 0.3.2

No intermediate version or DB migration is required. Keep existing server data and client registration state directories.

**Authenticated clients must change their configuration.** The `--token-file` option is no longer supported. Remove the option and its file path from MCP settings, place the token value from the existing file after `--token`, then restart the connection. A still-valid token does not need to be reissued. Unauthenticated connections continue without `--token`. Follow [README_AGENT.md](https://github.com/xelexis-dev/agent-session-broker/blob/main/README_AGENT.md) for examples and configuration storage.

After replacing server and client with 0.3.2, check versions, login, project lists and actual session replies. Tokens in files are neither read nor deleted automatically.

## 0.3.0 → 0.3.1–0.3.3

Back up all data, replace server and client with the target version, and keep the existing data directory. There is no need to delete the DB or import JSON again.

- Instance-wide unauthenticated access defaults to **allowed**, retaining existing behavior. Disabling it blocks existing unauthenticated projects too; prepare token authentication first.
- New tokens are fixed to their issuer as owner. Existing token owners are preserved. Except for Owners and system administrators, users may view only tokens they issued.
- Deleting a project permanently deletes its registrations, tokens and shared memory. Do not delete existing projects to test an update.
- Check `/healthz`, administrator login, project/token lists and actual session replies.

## 0.2.x → 0.3.x or 0.4.0

1. Replace the server executable or image with the target version and connect the **existing data directory**. If the working directory changed, specify an absolute `--memory-dir`. Replacing a Docker volume with a new empty bind directory hides the existing data.
2. Start the server. Management state migrates from `control/control.json` to `control/control.db`. When a project's registration store opens, valid JSON registrations under `projects/p-*/registrations` migrate to `registrations.db`. Do not assume all project DBs are created together immediately at server startup.
3. Log in with the existing administrator account and check projects and tokens. Management state v1 is converted to v2; an `admin` user and Owner permissions for each project are created. Existing token ownership is assigned to `admin`. Existing tokens that have never connected receive a 30-minute first-registration deadline from migration time; if expired, issue a new token in the panel.
4. Replace each PC's client with the same target version and restart MCP. Re-register sessions if necessary. After server restart, an on-disk registration alone does not establish liveness or readiness to receive.
5. Check `/healthz` returns `ok`, administrator login, project/token lists, memory reads in each project and actual replies between sessions. Restart once more and verify that management state remains intact.

Valid migrated JSON originals remain on disk, but **subsequent changes are written only to SQLite**. Migration-complete markers are saved in the same transaction as the data. Removing a registration from the DB and restarting does not revive it from old JSON. Do not edit JSON or delete only the DB to force reimport. Damaged management state may refuse startup, and invalid registrations may be quarantined; inspect logs and actual lists.

## Checking older installations, including 0.1.x

0.1.11 had a combined executable and a remote mode using project directories; these differ from 0.2.x management state and persistent registrations. 0.3.0 includes code to import valid existing `projects/p-*` directories after initial setup. This alone does not guarantee automatic conversion of every 0.1.x local-memory layout and registration record.

- Back up the original, then test with a separate copy as the 0.3.0 data directory. Complete `/setup` if no management configuration exists. Valid project directories are imported as unauthenticated projects, so review their access policy too.
- Registrations and undelivered messages held only in the old server's memory are not disk-migration inputs. Reconnect and register clients.
- Change MCP configurations that used an old launcher or combined executable to the absolute path of the new `asb-client`. Remove the old `client` subcommand before client options. Run the server as `asb-server`.
- If projects or memory are missing, return to the original and check the actual storage path and format first. Do not try to resolve it by running arbitrary intermediate versions in sequence.

## Failure and rollback

If a server that had data suddenly shows initial setup or an empty project list, check data mounts and startup logs before creating a new administrator. `/healthz` alone does not prove successful migration.

There is no reverse conversion from 0.3.x/0.4.0 SQLite to 0.2.x JSON, or a switch back from 0.4.0 to the earlier unauthenticated policy. Stop the new server and restore **the complete pre-update backup together with the executable version that made it**. Using only the JSON left after migration loses later changes. A complete backup restore also excludes changes made after the backup; preserve anything needed separately before proceeding.
