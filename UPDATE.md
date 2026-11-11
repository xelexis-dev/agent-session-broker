# Update history

English | [한국어](UPDATE.ko.md)

Earlier tags without release notes: 0.1.0–0.1.11, 0.2.0.

## 1.1.0 (2026-11-11)

- The control panel is available in English and Korean. Each account chooses its language in My profile. English is the default, so existing accounts start in English.
- Pages before sign-in follow the language last used in the browser (`asb_lang` cookie), and English without it.
- Every control panel page is translated, including live session updates, the session audit, administrator pages and session approval.
- Agent-facing text is English: MCP tool descriptions and results, scope labels, Management API errors, client diagnostics and command-line output. Machine codes and schemas are unchanged.
- The public guides (README_AGENT, UPGRADE_PATH and UPDATE) are English first, with Korean copies in `.ko.md` files. Release notes list English first, then Korean.
- Client 1.1.0 carries the English agent-facing text. Clients 0.17.3 and later keep working with this server.
- Data: the server adds one language setting per account. Existing data keeps its format. Rollback is unchanged: restore the complete backup together with the 1.0.4 server.

## 1.0.4 (2026-11-11)

- Behavior and data formats are unchanged from 1.0.3.
- The container image moved to `ghcr.io/xelexis-dev/agent-session-broker`. For Docker Compose, change `image:` in `compose.yaml` to this name. The previous image name, `ghcr.io/xelexis-dev/agent-session-broker-deploy`, is retired and will be deleted. Version 1.0.3 is also available as `ghcr.io/xelexis-dev/agent-session-broker:1.0.3`; older images are no longer provided. Older server executables remain in Releases at https://github.com/xelexis-dev/agent-session-broker-deploy .
- Client 1.0.4 changes only the version; behavior is unchanged.

## 1.0.3 (2026-11-11)

- Behavior and data formats are unchanged from 1.0.2.
- From this version, server Releases, npm client provenance and source come from https://github.com/xelexis-dev/agent-session-broker . Files for 1.0.2 and earlier remain available at https://github.com/xelexis-dev/agent-session-broker-deploy .
- The npm package's repository metadata changed, so client 1.0.3 is also published. The Docker image name is unchanged.

## 1.0.2 (2026-11-11)

- Behavior and data formats are unchanged from 1.0.1.
- Built with the Go 1.27.4 security release. The toolchain is included in executables, so npm client 1.0.2 is also published.

## 1.0.1 (2026-11-10)

- Behavior and data formats are unchanged from 1.0.0.
- Source code is published under the MIT license at https://github.com/xelexis-dev/agent-session-broker .
- The Go module path changed to `github.com/xelexis-dev/agent-session-broker/source`. This affects only building from source. Since the path is embedded in executables, npm client 1.0.1 is also published.
- Fixed the public repository check rejecting the public document list without the new Korean README during 1.0.0 publication.

## 1.0.0 (2026-11-10)

- First stable release. API, data formats and client behavior are unchanged from release candidate 0.20.2.
- Publishes server executables (macOS arm64·x64, Linux x64, Windows x64), container images (linux/amd64·arm64) and npm client `@xelexis/x-broker` 1.0.0 (Windows x64, macOS arm64·x64, Linux x64) together.
- From 0.20.x, update clients on every PC to 1.0.0 first, then stop the server, back up all data and replace the server. From 0.19.x or earlier, follow UPGRADE_PATH's `0.19.x → 1.0` steps: clients below 0.17.3 are rejected and `spawn_session` is removed. In-place downgrade is unsupported. After restoring a complete backup, revoke again credentials revoked after that backup.
- Rewrote the public README in English and added README.ko.md. The npm package guide is also in English.

## 0.20.2 (2026-11-09)

- Replaced control panel icons with unmodified Lucide icons (ISC; see `THIRD_PARTY_NOTICES`).
- Third-party notices changed, so npm client 0.20.2 is also published. Client behavior is unchanged from 0.20.1.
- 0.20.2 is a release candidate for stable 1.0. API and data formats are unchanged from 0.20.1. Update clients on every PC first, stop the server, back up all data and replace it. In-place downgrade is unsupported. After restoring a complete backup, revoke again credentials revoked after the backup.

## 0.20.1 (2026-11-09)

- 0.20.0 published only the npm client, without a server Release, GHCR image or public documents. 0.20.1 is the 1.0 release candidate providing both server and client.
- Aligned the npm version pin used for CI provenance verification with the pinned tool image and verify the actual version during image installation.
- **Breaking (0.20.1 RC / 1.0):** the server rejects all MCP tool calls from clients below 0.17.3 or without a version. The 1.0 client also requires server 0.17.3 or later; placeholder versions are no longer allowed. Update PCs in other projects too, then make a complete backup before replacing the server. Compat schema 2 and client inputs change, requiring a new npm client. Capability fallback for attachments/ACK and stored-data readers remain.
- **Compatibility change (1.0 preparation):** removed `spawn_session` and new `session:spawn` issuance. Upgrade startup atomically removes only `session:spawn` from stored token, grant and device-approval scopes (retaining control version 5). Data that would produce an empty scope list refuses startup without changes. Audit history and reading historical scopes remain compatible.

- Full-permission responses retain nonfunctional `session:spawn` until 2.0 for old-client compatibility. It is removed from storage, new issuance, editors and the scope catalog, and is not added to restricted responses.
- 0.20.1 is a release candidate for stable 1.0, providing both server and a new npm client. Update clients on all PCs first, then stop the server and back up all data. In-place downgrade is unsupported. After restoring a complete backup, revoke again credentials revoked after the backup.
- Removed temporary client-version inventory aggregation. Operators must check every PC's version in each project themselves.
- Removed old install.sh/install.ps1 distribution. Use public Releases for servers and npm for clients. Existing public history and split-archive launcher verification remain. Expanded instance-limit, backup/restore and HSTS guidance.
- Made release-candidate compatibility, upgrade/restore and container-promotion evidence verification mandatory. npm publication uses OIDC/provenance verification through a pinned GitHub Actions workflow.
- Automatically checks major control panel pages, open selects and confirmation dialogs with axe in light/dark themes. Improved Broker label contrast on setup/sign-in. Items without an automatic verdict remain for manual review.
- The software is provided under the MIT license starting with 0.20.0.

## 0.20.0 (2026-11-08)

- 0.20.0 published only the MIT-licensed npm client. No server Release, GHCR image or public documents were published. The server/client release candidate is 0.20.1.

## 0.19.3 (2026-11-07)

- Added **Client version inventory** to instance administrator settings: live registrations by version across all projects, clients below 0.17.3, unknown versions, compatibility rejections over 24 hours and last observation. It is memory-only and resets on restart; names, registration IDs and tokens are omitted. Observe for at least 48 hours before a future minimum-version increase and notify projects with old clients.
- Initial administrator setup requires a **one-time setup code** from the data directory. Already configured servers are unaffected. The public Compose example exposes ports only on 127.0.0.1. Keep setup codes and pre-setup backups secret.
- Added project memberships to My profile and applied the shared select component. Panel API tokens are issued as the signed-in account without password re-entry. Strengthened panel cookie, input and layout checks.
- Fixed server/client poll intervals accumulating delays after device approval `slow_down` responses. Supported existing clients retain their backoff behavior.
- **Publishes server and npm client 0.19.3 together.** The new npm client requires Node.js 22 or later; the panel supports current Chrome·Edge·Safari. Existing client 0.12+ and legacy connections, minimum server 0.12.0 and storage format (control version 5) remain. Stop the server and back up all instance data before upgrading.
- Stated noncommercial free use and separate written permission for commercial use in **LICENSE**. Full **THIRD_PARTY_NOTICES** and LICENSE accompany npm packages, server Releases, containers and public documents. The npm package contains 9 regular files; server Releases contain 4 executables and 2 notice files. Existing public packages and history remain.

## 0.19.2 (2026-11-06)

- Fixed `unsupported-harness` on Windows when Claude Code automatically updated while running and `/mcp reconnect` or a new MCP in that session started. Renamed Claude processes are recognized. **Update to client 0.19.2 and reconnect MCP.** Sessions newly started after the update were unaffected; before updating, exiting and resuming the session is a workaround.
- Parent-process identification failure blocks registration. `auth_status.parentDiagnostic` shows only the failed stage, error number and allowed program name, excluding paths, arguments and tokens.

## 0.19.1 (2026-11-06)

- In the VS Code Claude extension panel, broker messages appear as collapsed "Sender not recorded" peer rows with sender and body preview. Install npm client **0.19.1** and reconnect MCP to apply. Terminal CLI and other host delivery formats remain unchanged.
- Guides Claude to reply through **x-broker send_message** with the received registration ID instead of native SendMessage.
- Fixed sidebar logo/account icons and account menu breaking at tablet widths.

## 0.19.0 (2026-11-05)

- Shortened README into an introduction for general users and moved detailed agent instructions to README_AGENT.md.
- Added the **Management REST API** at `/api/v1` for projects, members, project tokens, sessions, device approval, audits, users and policy. `/api/v1/openapi.json` documents methods, scopes and JSON contracts for 41 operations. It is separate from MCP messaging tools.
- Management API is **OFF by default**. The panel provides an **API switch in Instance settings, API token list/issuance under My profile, and token listing/revocation in User details**. User API tokens are issued through the user's own sign-in or current-password verification; administrators cannot issue another user's tokens. Raw tokens appear only once.
- HTTPS is recommended. Plaintext API requests are rejected when **Allow HTTP connections** is off. Allowing plaintext may expose passwords and tokens along the network path.
- Use an ordinary account with restricted memberships and scopes for automation. **An administrator's write token acts as Owner in every project** and can add members. Device approval requires the separate `approve` scope, not selected by default.
- Creating users, promoting administrators, re-enabling users, setting any password and switching HTTP allowance OFF→ON require caller `current_password` reauthentication. Password, role or enabled-state changes invalidate existing user tokens. A crash may lose up to one minute of audit denial counts; a failure may lose one success audit stored separately from the change.
- Server, recommended npm client and installation examples are all **0.19.0**. The client includes a new minor compatibility row, so npm is also published. Minimum server/client 0.12.0 and legacy allowance remain. The Management API itself is a server feature; users install clients themselves as needed.
- **Downgrade warning:** user API tokens remain stored even when unused by 0.18.x and may be valid again after re-upgrade. For a leak, revoke the affected tokens before downgrading. If already downgraded, change that user's password. API OFF or downgrade alone does not replace revocation.

## 0.18.1 (2026-11-05)

- Added **Connecting to multiple projects** to README: per-project MCP entries, tokens and state paths, tool selection within a conversation, Codex startup connection and memory cost.
- The embedded client `setup` help and npm guide link directly to the multiple-project guide. Product behavior, including delivery, authentication and storage formats, is unchanged.
- Recommended client and install examples use **0.18.1**. Upgrade for the new embedded help; it is optional for existing 0.18.0 users. Minimum compatibility versions and the 0.18.0 attachment floor remain.

## 0.18.0 (2026-11-04)

- Recommended client: **0.18.0**. Client executables, tool guidance and reconnection handling changed; update the npm client and reconnect each conversation's MCP to apply. Existing minimum compatibility versions remain.
- **Stricter HTTP policy:** when HTTP allowance is off, plaintext MCP/OAuth requests are rejected even without forwarding headers. Localhost has no exception. Behind a TLS proxy, explicitly set `--trusted-proxy <CIDR>` and configure the proxy to overwrite a single `X-Forwarded-Proto` value instead of forwarding external input. Browser control panel access policy remains unchanged.
- Unresponsive registrations are detached in bounded batches after at least 30 days without activity and expiration of the probe deadline. Suspended registrations are excluded. Name/credential revocation follows existing policy and verified automatic-credential conditions; not every old name is immediately released.
- Codex observation reuses verified record locations/checkpoints; already verified hook identities avoid repeated discovery within a request. POSIX distinguishes the normal initial unverified steer-support state as `steer-unchecked`. Windows steer remains unsupported.
- Added delay and jitter when a receiver connection is rejected again immediately after successful resume, reducing rapid retries. Actual delivery and normal heartbeat progress reset the interval. This does not guarantee elimination of all network errors.
- Merely searching for handover targets no longer extends unrelated stored-session retention. Delivery progress JSON is cleaned after verified epoch retirement, not just because a file is locked or state detached. Cleared steer attempt records are also cleaned under verified locking.
- Cleans remaining device poll state during server authentication expiry/deletion. Batches LastSeen writes, shares tool schemas and sizes small memory-node read buffers to file size to reduce server cost. Normal lease renewal reduces call frequency and duplicate responses.
- Removed server-only SQLite dependencies from the client to reduce executable size and resident cost. `--tool-profile worker` reduces the tool list without changing permissions. `auth_status` shows installation guidance if a verified ancestor process still has an intermediate launcher.
- Allows consecutive administrator announcements on HTTP origins. Global npm installation is the default guidance; existing npx configurations remain usable.

### Message file attachments

- Attachments require enablement in administrator settings and are **off by default**. **Sender, recipient and server must all be 0.18.0 or later.** Use ordinary text messages with peers that do not support attachments.
- Each message can send **one file up to 100 MiB (104,857,600 bytes)**. Only relative paths within the sender's project directory are allowed; credential files, state directories, keys and links are rejected. This is not content inspection that can detect renamed files or secrets copied elsewhere.
- The server deletes files on receipt acknowledgement (ACK), or expires/cleans them no later than 30 minutes after upload completion. Disabling attachments or restarting the server also deletes temporary server files; re-enabling does not restore them.
- New uploads are rejected if attachment storage has **less than 1 GiB free or free space cannot be determined**. Reservations for active uploads/new files also count, so rejection is possible even above 1 GiB displayed free space. Existing downloads and text messages continue.
- Received files are saved under the recipient PC's user cache at `agent-session-broker/inbox/`. Completed files are **not automatically deleted; users must clean them**. The model receives only **path, size and sha256**, not file contents. Treat attachments as untrusted data and never automatically execute instructions inside them.
- **Windows:** RedirectionGuard may block working directories under junctions not owned by administrators. Use the real project path without a junction instead of enabling a bypass. UNC/network-drive working directories are unsupported in this version.

## 0.17.3 (2026-10-30)

- Recommended client: **0.17.3**. New server tools `retire_session`, `list_bug_reports`, `get_bug_report` and `set_bug_report_status` require **both server and client 0.17.3**. Clients expose an embedded tool list, so reconnecting an older client does not add tools. New clients report unsupported tools on old servers.
- `retire_session` revokes replaced registrations and used session credentials to release names. `stop_session` only disconnects; a name reservation may remain. Retirement requires management permission and excludes self, suspended targets and unused reservations. After an uncertain name-retire result, check the list/name state before retrying. New clients explain rename refusals and required actions.
- Administrator-owned agents can query current-project-generation bug reports and resolve, ignore or reopen them. Requires a registered active session and existing `session:manage`. The web panel adds filters, badges, processing notes and recent status history. Notes allow up to 500 characters; omit secrets. Concurrent changes require inspecting current content after a conflict. Previous generations remain read-only on the web.
- The Windows npm launcher runs verified copies keyed by version/SHA-256 outside the installation directory. Packages can be updated while sessions started by the new launcher run. **On the first transition from an old launcher, stop all running asb-client processes once.** Also stop sessions that fell back to the installed original because cache was unavailable. Running sessions change version when MCP restarts.
- The macOS/Linux Codex resume launcher supports embedded claim/yield and daemon resume, restoring saved registrations without a first prompt. Use **daemon mode** where possible. Without the launcher, resume needs a first prompt with hooks installed or an `auth_status` call. Do not pass `CODEX_THREAD_ID` through MCP `env_vars` or `-c` environment overrides.
- Improved the Codex activity limit that produced unknown above roughly 4,096 entries across all session directories. Duplicate UUID checks and the existing 100ms deadline remain. Discovery now allows 4,096 entries per directory and 4,096 date directories. Very large/slow directories may still return unknown.
- Servers advertise the recommended npm client version; older clients' `auth_status` gives harness-specific manual upgrade/reconnection steps. More frequent death checks while waiting for an old client on the same device reduce the previous extra 5–30-second wait. Live/unverifiable holders and other devices remain protected. Reconnection within 2 seconds is not guaranteed in every environment.
- `xbroker_docs` provides local upgrade, names, delivery, errors, reports and Kimi help without server connection, authentication or registration. Reopen Codex sessions to see new tools because their tool lists are fixed per session. Keep the original execution mode/flags; resuming a shared daemon alone does not prove a new MCP started. For Claude, change the configured version, reconnect through `/mcp`, then verify version, tool list and actual receipt.
- Durable connections negotiating message ACK provide **at-least-once redelivery up to host handoff while the server process lives**. This does not confirm model reading or replies. Handoff means Claude native inbox frame-write success, Codex queue success or hook-response write completion, or Kimi helper v2 acknowledgement after stdout write. `send_message.accepted` still means only queue acceptance.
- Unacknowledged messages are redelivered on reconnection/receiver failure. Completion metadata reduces duplicates, but termination between host handoff and metadata storage can repeat a delivered batch. Delayed ACK applies backpressure. On old-client downgrade, **issued but unacknowledged messages** protect their recovery copy with retryable `storage` for a fixed maximum of 10 minutes. Retries do not extend the deadline. After expiry, legacy delivery of those messages is allowed and returns to at-most-once. Empty registrations or those with only unissued pending messages have no such delay.
- **Loss exceptions:** server restart, retire, administrative detach, identity-invalid and suspension do not preserve in-memory messages. Old servers and connections without ACK negotiation retain their prior delivery behavior. ACK is neither a persistent queue nor a read receipt.

- Keep a complete server-data backup and previous executable before updating. ACK server queues are memory-only; rollback restarts the server and loses pending messages. Drain queues first where possible. Clients store completion-sequence metadata per registration/epoch without message bodies.
- Bug reports retain open/closed on disk. Version 0.17.2 reads resolved/ignored as closed, but rewriting with an older server may lose resolution, notes and status history, and reject status changes for records above 32KiB. Storage is capped at 64KiB; submission input at 32KiB. Retire audits keep the existing detached kind; no new scope was added. Rollback does not restore revoked credentials.

- A changed registration behind a previously contacted name blocks sends with `recipient-changed`. Inspect the current peer with `get_session` before sending. Listing sessions or repeating the request alone does not clear it.
- Bug reports can be deleted in the web control panel. Deletion history is stored separately; include it in full-data backups. Preserving deletion records on an older server does not restore actually deleted files.
- Fixed server shutdown delays while waiting for peer responses during TLS closure (#344).
- Diagnostics use v2 files; old records are read-only. Unknown extension fields are preserved; output includes only known codes. `auth_status` shows device-identification failure reasons and brief actions, without secrets or paths.
- Codex daemon delivers into active turns through `turn/steer` when ACK negotiation and lifecycle hooks are active. Enabled by default; disable with `--codex-steer off`. Generated responses may be rewritten, but running commands remain. Embedded and Windows retain tool-boundary, Stop and queue delivery.
- Automatic credentials whose new clients advertise reapproval support are revoked during stop/detached-registration cleanup. Old-client credentials retain the existing 7-day expiry. After revocation, the user must explicitly request `attach_session` for a new registration; previous peers must confirm the name's new registration.
- Batches detached-registration cleanup to reduce large-list costs and duplicate audits of the same detach.
- Improved receiver/ACK recovery after self-stop and explicit re-registration, epoch-change recovery, Linux Codex resume helper, project deletion with corrupt deletion-history files, and diagnostic-code compatibility/locking.
- **Known limits:** Windows does not support steer. Duplicates may remain on transitions to embedded/Windows and during the millisecond storage window in ordinary hook/queue handoff.
- Immediate daemon delivery (`turn/steer`) writes an attempt record without a body immediately before sending. After client restart or daemon stop, it checks conversation history and the Codex queue and does not reinject messages already inserted. Until verification completes, that message and later ones remain pending.
- Native Windows checks ran vet and the full Go suite on the final integrated tree under a restricted account. No product defects were confirmed; remaining failures were test assumptions/environment issues, including tests for a deduplication storage path unused on Windows. npm/script rechecks and Linux/macOS amd64 hardware validation follow separately after release.

## 0.17.2 (2026-10-25)

- Deleting and recreating a project with the same name no longer mixes old audit records or bug reports into the new project. Each project gets a generation ID separating record lists, counts, details, status changes and deletion. Only server 0.17.2 is required; existing clients need no update.
- Administrators can switch **Current records / History** in session audit and bug reports. Other generations are labeled Previous generation; records without a known generation, such as pre-upgrade records, show History (generation unknown). Historical generations are never inferred from names/times and attached to the current project. This is a server 0.17.2 panel feature.
- Only administrators can read history; project Owner/Developer roles cannot access even detail URLs. Historical bug-report status cannot be changed, and stored project names do not link to current projects. Unknown-generation records follow existing retention, with no separate deletion or merge-into-current button.
- With server and client 0.17.2, `auth_status` and tool errors distinguish project connection denials: `project_disabled` for disabled projects and `project_deleting` during deletion, rechecking at bounded intervals. Re-enabling a project allows normal resume.
- Denied requests distinguish `project_deleted` when a previously verified generation changed and `project_not_found` when the requested name is absent. Clients that verified capability support and the previous generation through authenticated responses stop automatic resume/reapproval for deleted projects and preserve stored registration. An absent name without a verified generation is not blindly called deleted; proxy HTML or a bare 404/410 without a code does not confirm deletion.
- Connections stopped by deletion can attempt recovery after changing token/server/project or explicitly requesting `attach_session`. They do not revive deleted registrations or automatically register in a new project with the same name. Generation information bound to the old credential is not carried into new requests. Both server and client 0.17.2 are required for this termination/recovery guidance.
- Added optional `expectRegistrationId` to `send_message`. Supplying a registration ID when replying or contacting a known peer rejects a changed name holder with `recipient-changed`, without enqueueing anything or automatically resending to the new holder. Existing name-only calls are unchanged; expected IDs are not automatically added. Requires server and client 0.17.2.
- Received messages include sender registration ID `fromRegistrationId`. Accepted-message origin stays unchanged if the sender later renames/disconnects, including batch, Claude and Codex delivery. Existing `registrationId` still means recipient ID. Looking up a changed peer's current registration requires `session:read`; `message:send` alone receives only the refusal code. New input/origin handling requires both server and client 0.17.2.
- Prevents two clients with cloned state directories from alternately taking the same live registration. With server/client 0.17.2, a registration used by another receiver reports `registration-in-use` and waits 5, 10, 20, 30 seconds, then every 30 seconds. It neither deletes stored registration/credentials/messages nor creates a new registration. Distinct receiver clients remain distinguishable under the same Claude/Codex parent.
- A live registration can be inherited once the previous receiver on the same device is confirmed dead. Different device evidence or unverifiable process death requires waiting for connection expiry. A still-renewing process is not displaced just because 30 seconds passed. Server 0.17.1 lacks this protection.
- Even on server 0.17.2, reconnecting between 0.17.1 clients (legacy→legacy) retains 0.17.1 behavior without protection against cloned-state races. New clients cannot immediately take registration from a live 0.17.1 receiver. Conversely, a 0.17.1 client refused by a new receiver does not automatically wait and may need `/mcp` reconnection.
- Fixed delayed MCP initialization/tool listing while waiting for parent-conversation discovery, registration or local locks when the same Claude conversation is open in two processes, such as tmux. The client's ancestor process selects its conversation; another client's lock on the same local state is preserved and reported in use. Client 0.17.2 alone applies this fix. `npx @latest` lookup/download delays are separate: pin a published MCP version and prepare it with that version's `--version` first.
- Fixed returning from Claude `/restart`'s temporary startup conversation to a saved conversation inheriting registration but lacking native inbox access until `/mcp reconnect`. Delivery verifies the current parent's conversation/process and actual inbox peer, never another process or stale connection. Client 0.17.2 alone is sufficient.
- Fixed misleading `identity-conflict` after panel detachment stopped automatic reapproval. If registration is absent on the server, clients now report `registration-not-found` and possible administrator detachment/removal. No registration is automatically recreated; request `attach_session` to use it again. Requires server/client 0.17.2.
- Existing project-token permissions can be edited in the panel without changing the token value. Choose permissions in the token list's Change permissions panel, review additions/removals and apply. Approved session credentials update too, without MCP configuration changes or session reapproval. Only project Owners/instance administrators can add permissions; Developers can only reduce their own token permissions. Approval mode, automatic registration limit and lifetime cannot change. Disabling receipt also prevents that token's sessions from sending. Changes are audited.
- Fixed connections stuck at `identity-conflict` after token/project changes even when `attach_session` was requested again. Explicit attach clears stopped recovery and stale approval requests before issuing anew. A new token with the same owner inherits stored registration; a server-confirmed different owner reports `registration-owner-mismatch` without taking it. Approval waits beyond 10 minutes report `pending-expired`. Client 0.17.2 alone applies this fix.
- Fixed explicit `attach_session` takeover of an administratively detached registration returning `handover-refused` if the old receiver had already saved detached state. Explicit takeover/connection requests can revive detached registrations. Automatic handover after an in-process conversation change such as `/clear` cannot. Client 0.17.2 alone is sufficient.
- Reduced server load with many sessions by removing full-session queries on each renewal and unnecessary copies during expiry cleanup; a 100-session measurement showed substantially lower server CPU. Panel CSS/JS/live data use gzip, and versioned static files are browser-cached. Clients batch normal renewal records to reduce writes and read only the needed prefix of Codex conversation files.
- Security update: built server/client executables with Go 1.27.3 and current `golang.org/x/crypto`·`x/sys` to address known standard-library vulnerabilities. Upgraded the container base to Alpine 3.23.7 and pinned build images by digest.
- Supports x-broker in Kimi Code CLI (verified on 2.3.1). Add three `kimi-hook` hooks (SessionStart, UserPromptSubmit, Stop) to Kimi `config.toml`, then register with `attach_session` and exchange messages; README has examples. Kimi cannot externally awaken idle conversations, so messages arrive at the next prompt or active-turn end, once per turn. Its session list therefore shows automatic delivery off, and accepted sends return a delay warning. Requires both server/client 0.17.2; old servers produce `unsupported-server` before registration attempts.
- Running the client outside Claude Code, Codex or Kimi Code now gives `unsupported-harness` instead of unexplained `invalid argument`; `auth_status` reports the same state. Client 0.17.2 alone is sufficient.
- Fixed activity remaining `unknown` because of Codex 0.164 `additional_tools` history entries or late completion records for prior turns. Client 0.17.2 alone is sufficient.
- Fixed messages stuck until the next user input when a Codex turn ended without Stop, such as on model error. Once the client verifies the turn ended in conversation history, it hands pending messages to Codex. Requires hooks to pass `turn_id` (see README). Client 0.17.1 rejects this field: upgrade to 0.17.2 before changing hooks.
- Fixed Claude reopened in a new process with `claude --resume` receiving but unable to send due to `reconnect-required` until `/mcp` reconnection. The block clears automatically when the same parent and active connection are verified. `auth_status` distinguishes inbox problems from pending conversation-follow verification. Client 0.17.2 alone is sufficient.
- Server 0.17.2 works with client 0.17.1. Record isolation and panel changes need only the server update; project termination guidance/expected peer IDs need a client update too. Running older clients do not gain new inputs/delivery fields. Client 0.17.2 with server 0.17.1 lacks project termination detection/receiver protection and rejects explicit `expectRegistrationId` as `unsupported` without dropping the expectation and sending. With both at 0.17.2, re-enabling receipt resumes after the next successful renewal/normal recovery without restart. Server 0.17.2 with client 0.17.1 applies reduced permissions immediately, but re-enabling receipt requires client restart with the same saved registration. Client 0.17.2 with server 0.17.1 lacks the new permission-observation signal and retains the old persistent receipt refusal.
- Update order: **full instance-data backup → server 0.17.2 → npm client 0.17.2 → reconnect each conversation**. Stop/back up the server, then start with the same data path. First startup assigns existing projects generation IDs once and raises `controlVersion` to 5; IDs survive restart/backup restore. Change the MCP npm version and reconnect Claude `/mcp` or restart Claude/Codex. Cross-version write compatibility is not guaranteed. Rollback requires the previous executable and complete pre-upgrade data, including control/registration/bugreport, not only `control.db`. Starting server 0.17.1 directly on 0.17.2 data without restoring a backup refuses startup with `control panel storage unavailable` and leaves data unchanged.

## 0.17.1 (2026-10-23)

- Supports Claude background sessions: `claude --bg`, viewers attached with `/resume`, and `claude --resume` after `claude stop`. `claude --resume` or `/resume` in a newly started Claude also retains registration/name without `/mcp` operations.
- With both server/client 0.17.1, `/clear` hands registration to the new conversation; `/resume` back takes it again. Returning to the original conversation restores connection even after refused handover. Old servers hold messages instead of handing over, delivering after return. Exiting Claude before returning loses those pending messages.
- Manual-approval tokens support conversation handover. After `/clear` or a conversation change, the user requests connection and approves, retaining registration name, ID and pending messages. Approval shows temporary `<name>-next`, replaced by the original afterward. No automatic handover without approval; credentials pinned in configuration are unsupported.
- New conversations register only on user request. When an agent calls `attach_session`, automatic-registration tokens register without approval; ordinary tokens show an approval code. Existing conversations reconnect automatically.
- `session:manage` can disconnect, suspend and resume other manageable sessions. Owners manage all project sessions; Developers only same-owner sessions. Self-management allows only disconnection. `suspend_session`/`unsuspend_session` need client 0.17.1; existing `stop_session` can disconnect other sessions with only server 0.17.1.
- `auth_status` queries current server credential permissions/descriptions. Older servers or failed queries use stored information, distinguished by `scopeSource`.
- Added `report_bug` and administrator-only bug-report lists/details. Clients attach version, OS, authentication/receiver state and up to 20 recent events, excluding tokens, paths and message bodies; servers also redact x-broker tokens. Limits: 10 per registration/hour, 200 per project/day, at most 2,000 per instance for 180 days. Project deletion removes reports. Administrators can close/reopen. Requires server/client 0.17.1.
- Fixed unnecessary reconnection of all project sessions roughly 10 minutes after registration during automatic-registration cleanup. The server fix also applies to 0.17.0 clients.
- Increased the automatic-registration burst allowance to 10 requests. Limits give the exact delay in `Retry-After`.
- Improved reconnection after automatic-credential denial/expiry and project-token replacement. Existing registrations continue; administratively detached registrations are not automatically recreated. Improved recovery after disconnection, reapproval and conversation changes.
- Messages accepted after a client crash but before lease expiry (roughly 30 seconds) are retained and delivered if the same registration reconnects within 10 minutes. Server restart still loses pending messages. This server fix also applies to 0.17.0 clients.
- Clients started while a server is down/unresponsive retry with increasing intervals until it returns instead of failing immediately. Authentication refusal/configuration errors still stop immediately.
- Corrupt/unreadable client state produces fixed explanatory guidance without file contents or paths.
- Session names reject invisible characters, including zero-width and directional controls. Existing registrations with such names can still connect and rename.
- Double-submitting or resubmitting the panel's project-token form through browser Back issues only one token. Existing scripted issuance remains unchanged.
- Project deletion also deletes session audits so a recreated same-name project cannot show old records. The first post-upgrade save also cleans audits left by already deleted projects.
- One corrupt project's data does not prevent server startup or other projects from working. Only that project is unavailable; its files remain unchanged.
- A corrupt bug-report store no longer prevents startup; only bug reporting is disabled. Restore valid files and restart to recover. Deleted projects' reports are omitted from lists.
- After a pause longer than 30 seconds, such as laptop sleep, clients still deliver messages received just before the pause instead of discarding them.
- Correctly computes server-supplied retry delays even when PC and server clocks differ.
- Automatic-registration limits now report the exceeded limit and remedies (detach unused sessions or raise the limit in the panel) instead of `identity-conflict`.
- A project isolated for corrupt data returns HTTP 503 with the cause and administrator backup-restore guidance. Its panel settings, sessions and audit pages show the same state.
- Concurrent settings saves no longer silently overwrite earlier changes: conflicts show current values. Applies to project access, user access, profile and instance policies; scripted calls remain unchanged.
- Server 0.17.1 works with client 0.17.0, which retains its running tool list and needs an update for new tools. Client 0.17.1 also connects to server 0.17.0, without `/clear` handover, other-session management, current server-permission queries or `report_bug`.
- Update order: **full-data backup → server 0.17.1 → npm client 0.17.1 → reconnect each conversation**. Stop/back up the server, then start with the same path. Running sessions automatically reconnect with the same registration roughly 1 second after server return, potentially longer depending on environment. Change the MCP npm version and reconnect Claude `/mcp` or restart Claude/Codex. Rollback requires the complete pre-upgrade backup and previous executable together.
- Clarified name-retention differences between `stop_session` and panel disconnection in `attach_session` guidance.

## 0.17.0 (2026-10-18)

- Inter-session messages include broker acceptance time `sentAt` in UTC with millisecond precision. Requires server/client 0.17.0 and negotiated `message.sentAt`. Clients 0.12–0.16 keep their old shape without the field. Update npm clients to use this feature.
- Added unlimited options for registration count and lifetime on project tokens registering without approval. Finite ranges are 1–100000 registrations and 1–36500 days; defaults remain 8 and 30 days. The instance switch permitting restricted/automatic-registration issuance remains. **After issuing any unlimited token or using values beyond the old 64-registration/90-day range, rollback to 0.16.0 or earlier requires restoring a complete pre-upgrade data-directory backup.**
- Separately manages server/client compatibility policies. Server 0.17 requires client 0.12.0 minimum and still allows legacy clients; client 0.17 requires server 0.12.0. New clients detecting incompatibility stop automatic-registration retries, retain only `auth_status`/session queries and give upgrade/reconnection guidance.
- Per-conversation client session/registration state is cleaned 30 days after last use; diagnostics after 14 days. State locked by running connections is skipped. Cleanup runs in the background after startup at most once daily.
- The npm launcher replaces itself with the client executable on supported POSIX/Node.js environments, reducing resident processes/memory. Windows and unsupported Node.js retain the existing launch path.
- Tidied Instance settings, announcement results and profile layout, reducing redundant guidance. Session PID appears beside its name; conversation/PC identifiers below it are hidden. All text uses Pretendard Variable. Disabled token-scope selection gives a short reason and administrator-setting guidance.
- Removed the old combined local executable and local session spawning. Use the server/npm-client connection. Hidden remote `spawn_session` remains for old-client compatibility, returning guidance that the server does not start processes.
- Removed legacy JSON control-store writes in favor of SQLite. First-run import from existing JSON and SQLite upgrade paths from 0.15/0.16 remain. Control format stays v4, but **back up the entire data directory before replacement**. Also read 0.15.0 upgrade guidance when coming from 0.14.0 or earlier.

## 0.16.0 (2026-10-16)

- Session registration includes an approval link with prefilled code/requested nickname. Verify and approve there; sign-in returns to the same page. Enter a nickname on the approval page if none was requested.
- Reduced the model-visible tool list: hides `wait_for_message`, `ingress_hook` for confirmed Claude sessions, and `spawn_session` for remote servers without spawning support. Existing calls remain available despite hidden listings.
- `send_message` failures distinguish absent recipients, lookup failure and sender identity mismatch. Remote connections omit session-start guidance.
- Improved macOS Claude resume when conversation identity changes before registration. Briefly waits for identity stability, then prefers an exact saved-registration match to reduce unnecessary approval. Verifies that the initial inbox in the same process follows `/resume`/`/clear`; already registered connections keep their identity. If inbox verification fails, session queries remain but sending/receiving stop with `/mcp reconnect` guidance.
- `auth_status` no longer labels configured session credentials as legacy.
- Verified that existing clients automatically reconnect after server replacement with unchanged registration/name and no user action. Clients stay alive if tools fail during downtime. Failed sends are not automatically retried; messages pending before restart are not preserved.
- Control database format remains v4. Coming from 0.14.0 or earlier, **back up the entire data directory first** as described for 0.15.0.

## 0.15.0 (2026-10-14)

- Project tokens support scoped permissions. Restricted and approval-free registration tokens require the instance administrator to enable issuance, off by default. Approval-free tokens default to 8 sessions (1–64) and 30 days (1–90); indefinite issuance is forbidden.
- Added panel audit history for approval-free registration requests, issuance and registrations.
- The panel sidebar displays the current server version.
- Session lists display model and reasoning effort; unobservable information shows unknown.
- `auth_status` displays MCP client and connected-server versions.
- Trims surrounding whitespace from approval codes and nicknames.
- Fixed incorrect temporary-conversation identity during Claude resume, allowing existing approval/registration reuse.
- Activity updates again when fresh evidence follows unknown due to stale observations. Improved compatibility by falling back to existing approval when old servers lack new registration support.
- Preserved session-list/detail output compatibility for older clients. Send-only sessions without receive permission can still use permitted features.
- Clients for all OSes, including Windows, are distributed only through npm `@xelexis/x-broker`. GitHub Releases provide only 4 server executables. npm packages do not include provenance; package integrity/size and embedded client checksums are verified.
- Control database storage upgrades to v4. **Back up the entire data directory** before replacing the server. Restore that pre-upgrade backup when rolling back to an older server.

## 0.14.0 (2026-10-11)

- Project members retain session ownership and can disconnect their sessions after token history cleanup.
- Revoked/expired credentials give clear causes and recovery steps. Request a new approval code with `attach_session` or replace an existing single-session token, preserving registration/name.
- Improved reapproval after credential refusal, fixing duplicate approval queries and stalled re-registration.
- Existing data/accounts remain. Back up the data directory before server replacement.

## 0.13.0 (2026-10-10)

- Added session activity indicators: supported Codex conversations show Working, Idle or Unknown; Claude currently shows Unknown. Activity is separate from connection state.
- Activity tooltips show observation/change times in local time; session activity updates automatically.
- After required authentication, sign-in returns to the originally requested page.
- Consistent keyboard focus outlines on buttons and inputs.
- Connection refusal gives allowed reasons and safe guidance without raw internal errors or sensitive information.
- Fixed session selection in the legacy detach path.
- Project-token issuance caps count only active, unrevoked, unexpired tokens. Revoked/expired history is kept 30 days from that event, then cleaned. Project-token approval counts and latest-registration summaries remain.
- Project members can disconnect their own sessions; Owners can disconnect all project sessions.
- Existing data/accounts remain. Back up the data directory before server replacement.

## 0.12.0 (2026-10-09)

- Session status explanations are available in tooltips. Removed the redundant project column from within-project session lists.
- Long-unresponsive sessions remain registered as missing. Reconnection or reapproval after credential expiry retains ID/name. Lack of probe response does not automatically delete them. At the registration cap, new registrations are refused; administrators can detach old sessions.
- Clients record renewal failure, receiver exit, reconnection and auth changes in bounded per-conversation diagnostic files. auth_status shows last renewal success and failure summaries. No tokens, URLs, arguments, message bodies or raw errors are recorded.
- Unknown static assets return 404 instead of a sign-in page.
- npm publication waits for registry propagation and verifies published packages, without republishing the same version because of propagation delay.
- Existing data/accounts remain. Back up the data directory before server replacement.

## 0.11.0 (2026-10-08)

- Reapproval after session approval removal/refusal now connects with fresh credentials.
- Connected-session lists show nickname reservations without registrations. Authorized users can release just the reservation and approve the same name again.
- Compact session statistics appear within project management; unregistered nickname reservations are counted separately.
- Aligned the Project settings menu icon with the settings icon.
- Instance administrators can announce to connected sessions ready to receive. Sender name broker-admin is reserved; duplicate acceptance of the same request is prevented. Acceptance does not confirm actual model receipt.
- Interrupt-handling failures during delivery are reported through the affected session's connection.
- Existing data/accounts remain. Back up the data directory before server replacement.

## 0.10.0 (2026-10-08)

- Changed the panel font to Pretendard and applied a teal theme with light/dark support.
- Improved mobile sign-in layout and connection diagram, showing Claude/Codex connected to a Project through the Broker.
- Sidebar stays visible while scrolling. Aligned header dividers, session cards, buttons and scrollbars.
- Selecting a user row opens details to change permissions, enabled state and password. Password changes require confirmation input.
- Applied shared selects and enabled switches. Searchable selects work where search is enabled.
- Fixed stale CSS cache causing mismatched sign-in input/button heights.
- Unified MCP configuration guidance on npm, using `command = "npx"` on every supported platform, including Windows.
- Existing data/accounts remain. Back up the data directory before server replacement.

## 0.9.2 (2026-10-04)

### Single npm package and automatic publication

- One `@xelexis/x-broker` package contains Windows x64, Linux x64 and macOS Intel/Apple Silicon clients. Run with `npx -y @xelexis/x-broker@0.9.2 --version`.
- After stable-release executable verification, compares client inputs with the last npm publication. Publishes automatically only if client code, shared dependencies, packaging or build tools changed; server/web-only changes retain the npm version. Public Releases provide 8 client/server executables and 1 npm bundle.
- Aligned npm repository/homepage/support links and executable module paths with the public distribution repository. File content and integrity are checked before publication.
- Improved distribution checks to verify npm executable permissions accurately under restrictive file-permission settings.

### Update guidance

- Existing data formats and credentials remain. Back up the data directory before replacing server/client.
- This release improves distribution; it does not include the web redesign.
- 0.9.1 stopped during release preparation and is not publicly packaged. Existing 0.9.0 distribution/history remain.

## 0.9.0 (2026-09-24)

### Renaming connected sessions

- Ask Claude or Codex `Change my x-broker nickname to home-worker-3` to rename your session immediately, without administrator reapproval.
- Invalid names and names already used/reserved in the same project are rejected. The old name is not retained as an alias; send to the new name afterward.
- Registration ID, conversation, receiver and pending messages remain. Reconnection/server restart keeps the new name; messages accepted before rename retain their original names.
- If storage/connection errors leave the outcome uncertain, inspect the current name in the session list and request the same new name again.
- Update both server and every PC's client to 0.9.0 and reconnect MCP to use the new tool. Existing data/credentials remain; follow upgrade guidance for order and backup.

### Reconnection and delivery improvements

- Prevents clients getting stuck retrying reconnection with registrations removed from the server.
- Repeated renames/client restarts preserve original name information and order for messages accepted before rename. Renaming alone does not replace the receiving lease.

## 0.8.3 (2026-09-16)

### Preventing duplicate issuance on project-token refresh

- Fixed refreshing the token-issued page repeating issuance. Issuance now redirects to settings; subsequent refresh creates no extra token.
- Raw tokens appear once only in the issuing sign-in session/project, without mixing other tabs' results or placing raw tokens in URLs/cookies.
- Applies by updating the server executable/container. Existing tokens, registrations, data formats and protocol remain. Close issuance-complete pages opened before updating and reopen Project settings.
- Already duplicated tokens are not automatically revoked. Inspect/revoke unused ones in Project settings.

## 0.8.2 (2026-09-16)

### Credential persistence and connection ordering

- Session credentials obtained with project tokens are used only after local persistence. Concurrent tool requests cannot start a connection with credentials still being saved.
- Approval status remains pending during persistence. Failed storage reports failure without connecting; successful storage continues automatic registration.
- Existing project tokens, registrations and client state remain. Replace the client executable and reconnect MCP to apply; server storage/protocol are unchanged.
- For the panel redesign and instance HTTP policy, see [0.8.1 changes](https://github.com/xelexis-dev/agent-session-broker-deploy/releases/tag/0.8.1).

## 0.8.1 (2026-09-16)

### Control panel redesign

- Searchable project lists open current session details; the top gear opens Project settings. The bottom user menu provides My profile, sign-out and administrator-only panels.
- Project settings shows only project tokens, distinguishing last use, approval count/latest registration and In use/Registered/Waiting states. Nickname-bound legacy session tokens are no longer issued, but authentication remains. Open session approval directly at `/auth/code`.
- **Administrator panel → Instance settings** centrally manages HTTP allowance. Changing it makes all project connections recheck policy.

### Update guidance

- Updates directly from publicly distributed 0.7.0. Version 0.8.0 was not publicly distributed. 0.8.1 includes the panel redesign and a mock-server fix for release reapproval tests.
- Stop the server and back up the entire data directory, including DB and any journals. Downgrade requires restoring the previous executable and complete pre-update backup together.
- If at least one non-deleting project exists and all previously allowed HTTP, the instance allows it too. Mixed policies or no eligible projects default to denied; administrators must check after updating.
- Existing project-token history is supplemented from remaining session credentials. History of already deleted credentials cannot be recovered. Subsequent usage/registration history survives session-credential expiry.
- Retains the 0.7.0 client protocol. New screens/instance policy apply after updating the server.

## 0.8.0 (2026-09-16)

### Project-centered control panel

- Reorganized the workspace sidebar as a searchable project list. Selecting a row shows connected sessions; the top gear opens Project settings.
- The bottom user menu provides My profile/sign-out. System administrators use the administrator panel for users and instance settings.
- Project settings issues/manages only project tokens, showing last use, registration history and In use/Registered/Waiting states. Removed nickname-bound session-token issuance screens; previously issued credentials still authenticate.
- Session approval uses the client-provided `/auth/code` URL instead of the sidebar. Choosing a nickname during approval remains.

### Instance HTTP connection policy

- Moved HTTP allowance from project settings to **Administrator panel → Instance settings**. Saving makes all active project connections recheck policy. HTTPS can reconnect; HTTP reconnects only when allowed.
- During upgrade, the instance allows HTTP if at least one non-deleting project exists and all allowed it. Mixed policies/no eligible projects start denied. Administrators using HTTP must check instance policy after updating.

### Data migration and application

- Stop the server and back up the entire data directory before updating. New storage fields mean downgrade requires restoring the previous executable and complete pre-update backup together.
- Existing project-token registration history is supplemented from remaining session credentials; history of deleted credentials cannot be recovered. Future usage/registration history stays on project tokens after credential expiry.
- The 0.7.0 client protocol remains. New panel/instance policy are provided by server 0.8.0.

## 0.7.0 (2026-09-16)

### Project-token HTTP allowance policy

- System administrators can enable **Allow HTTP connections** in Project settings. Default denied, stored independently per project. Ordinary project Owners/Developers/Viewers cannot change it.
- Allowed projects can request approval codes using an `http://` server address/project token, then connect with session credentials. No separate client HTTP option is needed.
- Clients query server policy without a token before sending credentials. Denial, lookup errors, unsupported servers or redirects prevent credential transmission. Reconnection with stored credentials checks the same policy.
- Turning HTTP allowance off reconnects active project connections and blocks HTTP without revoking project tokens/session credentials themselves. Existing HTTPS proxies and nickname-bound session-token connections remain.

### Application and limits

- HTTP project-token connections require both server/client 0.7.0 or later. Update server first, replace clients and reconnect MCP.
- HTTP does not encrypt tokens; allow it only on a separately protected trusted network. This is ASB client HTTP policy, not server-authenticity verification or a substitute for TLS/firewalls. HTTP support is an ASB exception to RFC 8628's TLS requirement.
- Codex CLI 0.154.0 `exec` consumes received messages at the next turn's start. Interactive TUI consumption timing was not verified.

## 0.6.0 (2026-09-15)

### Project tokens and session approval

- Create nickname-free **project tokens** (prefix `asb_p_`) under **Project session management → Issue project token**. One token can be shared in multiple harness/conversation MCP configurations.
- Project-token clients request an approval code per conversation. At **Session approval** (`/auth/code`), users enter it, verify harness/PC/conversation, choose a nickname and approve/reject. Only the issuer, project Owner or system administrator can approve. Used nicknames are rejected with a warning. Codes last 10 minutes; input attempts are limited to 10 per user/minute.
- Approved sessions connect to MCP only with **session credentials** prefixed `asb_s_`, expiring 30 days after issuance or after 7 days unused. Revocation/expiry closes open MCP connections. Revoking/disconnecting an approved session affects only that session; revoking its project token revokes every session approved with it.
- Existing nickname-bound session-token issuance/connections remain.

### Client

- A project token in `--token` enables approval mode. `--broker-url` must be HTTPS; project tokens are sent only for approval-code requests/exchanges.
- Local MCP starts immediately before approval and lists tools. New `auth_status` and broker tool responses show approval URL/code. Approval polling runs separately at standard intervals; received credentials are used only after disk persistence.
- One client process handles multiple conversations with separate credentials/connections. State under `--auth-dir` (alias `--state-dir`) at `sessions/<key>/conn.json` restores pending approval or saved credentials on restart. Existing session-token registrations for the same server/project continue through ownership-proof handover.
- Denied, expired or panel-detached/revoked credentials stop without automatically requesting new codes. Ask `Register this session with x-broker` to obtain a new code through `attach_session`. Project tokens against older servers without approval support stop with server-upgrade guidance.
- Actual harness verification: Claude Code 2.1.270 (`-p`/`--resume`) and Codex CLI 0.154.0 (`exec`/`exec resume`) covered pre-approval guidance, approved registration/tools/bidirectional messaging, restart recovery and stopping after detach. Codex `exec` consumes messages at the next turn, without injection into active turns. TUI consumption timing was not checked.
- Authentication/MCP requests do not follow HTTP redirects, preventing credentials from reaching other addresses. Code-request IDs are persisted before requests, avoiding duplicate pending approvals after response loss/retry. Fixed clients launched in another Claude session's terminal/tools incorrectly binding to the outer session.

### Session disconnection and token lists

- Panel **Disconnect** revokes the connection token and removes registration, immediately releasing the nickname. Opening session management cleans only registrations left detached by older versions, retaining their tokens.
- Revoked tokens are hidden from lists but remain rejected for authentication.

### Compatibility

- Upgrade directly from 0.5.1 without mandatory intermediate versions. Existing session tokens, MCP configuration, server data and client state remain. **Update server first**, then replace clients and restart MCP connections.
- Management storage changes to Version 3; downgrade to 0.5.x or earlier requires the complete pre-update backup and matching executable together.
- Project-token approval requires an HTTPS broker address. The server still listens on HTTP, so place a TLS-terminating reverse proxy in front. Existing HTTP/session-token connections continue working.

## 0.5.1 (2026-09-14)

### Automatic-registration guidance cleanup

- Removed legacy `register` MCP prompts and embedded skills/resources from client/server because they confused automatic-registration guidance.
- Removed related `skills/list`, `skills/get` and skill-extension declarations.
- Initial guidance now describes automatic registration through token authentication. Automatic registration, reconnection and receiving remain, as does user-requested recovery with `attach_session`.

### Compatibility

- Upgrade from 0.5.0 without mandatory intermediate versions or DB migration. Existing tokens, MCP configuration and server/client state remain.
- Update server/client and restart MCP to remove legacy registration guidance. For explicit recovery, ask `Register this session with x-broker`.

## 0.5.0 (2026-09-13)

### Session suspension

- Added **Suspend** and **Resume** to project session management. Blocks a session's MCP communication while preserving registration, nickname and token association.
- Suspension survives server restart and excludes the registration from offline cleanup. Suspending discards messages pending on the server.
- New clients reconnect automatically after resume; once ready, they receive only new messages.

### Control panel improvements

- Made table action buttons compact/consistent and improved layouts for long nicknames/narrow screens.
- Aligned user-management input columns and project-member actions; unified clickable areas in profile project-role lists.
- Distinguished first-registration token history from current session state and removed unnecessary disconnect buttons on already detached sessions.
- Confirmation now shows targets and effects for token revocation, member removal and role demotion.
- Fixed ignored input when rapidly closing/reopening confirmation dialogs with the keyboard.
- Fixed rapid duplicate submissions hiding successful token issuance/project creation behind an error screen.

### Compatibility

- Upgrade from 0.4.0 without mandatory intermediate versions. Update both server/client for automatic reconnection after resume.
- Data directory and token configuration remain. Rollback requires restoring the complete pre-update backup too.

## 0.4.0 (2026-09-13)

### Token authentication only

- Unified all server project access on token authentication. Removed project unauthenticated mode and the administrator's unauthenticated-access switch.
- Existing unauthenticated projects migrate to token authentication, preserving accounts, members, tokens, registrations and shared memory. Client `--token` is required.

### Automatic registration after MCP connection

- Once the current conversation is verified, MCP automatically registers it with the token's nickname. No separate tool call is needed for initial registration.
- Where Codex lacks conversation ID at MCP startup, the first ordinary tool call registers automatically before processing. IDs are never guessed to bind tokens to another session.
- Existing registrations recover through ownership proof; explicitly detached/deleted registrations are not automatically revived. Handles lost registration responses and duplicate initialization/shutdown races.
- Legacy Claude channels distinguish registration from readiness. Before channel activation, registration stays connected but receives no messages; reception starts after activation.

### Compatibility and upgrade

- Existing unauthenticated users need token issuance/MCP changes. Explicitly register once in the same conversation to bind a saved unauthenticated registration to a new token.
- Valid supported data migrates directly without mandatory intermediate versions. Keep existing server-data/client-state directories.
- README covers installation/authentication; UPGRADE_PATH covers version transitions and verification.

## 0.3.3 (2026-09-13)

### Control panel usability

- Successful saves/creation/changes display notifications that dismiss automatically or through a close button.
- **Add user** opens a creation modal. Errors reopen it preserving username/role without redisplaying passwords.
- My profile's project roles now match the project-list layout with links to Project settings.
- Member addition moved into a modal with up to 10 similar enabled-user suggestions as a username is typed. Existing members are excluded; keyboard/mouse selection is supported.
- Separated member-role change/removal buttons. Add requests cannot overwrite existing roles; last-enabled-Owner protection remains.
- Unified spacing/presentation of buttons, forms, lists and dialogs, with layouts usable on small screens.

### Compatibility and upgrade

- Retains 0.3.2's direct `--token "token value"` input. Existing tokens/client configuration continue unchanged.
- No DB-format change, new migration or mandatory intermediate version. Existing accounts, projects, member permissions and registrations remain.
- Updated README panel instructions and UPGRADE_PATH's 0.3.3 route.

## 0.3.2 (2026-09-13)

### Direct client token input

- Token-authenticated connections now use `--token "token value"`. Enter issued tokens directly in Claude/Codex MCP configuration; no separate token file is needed.
- Removed `--token-file`. Old configurations receive guidance to switch; existing files are neither automatically read nor deleted.
- Empty tokens, whitespace/control characters and overlong values are rejected before connecting. Invalid CLI-option errors do not expose input values.
- Token-free unauthenticated connections and server Authorization-header authentication remain. Token ownership, nicknames and existing registration state are unchanged.

### Documentation and upgrade

- Updated README authentication examples for direct token input.
- Added the 0.3.2 path and required configuration changes for existing authenticated clients to UPGRADE_PATH. No DB migration or mandatory intermediate version.

## 0.3.1 (2026-09-13)

### Project deletion and access policy

- Project Owners/system administrators can delete a project after confirming its exact name. Related registrations, tokens, shared memory and other data are permanently deleted too.
- Blocks new requests during deletion and settles active data operations before removing storage. Interrupted deletion can be retried in the panel.
- Added an instance-wide unauthenticated-access switch in the administrator menu. Disabling it blocks existing unauthenticated projects and makes new projects use tokens. Existing installations retain unauthenticated allowance by default.

### Session token permissions

- New session tokens are owned by their issuer; removed owner selection. Nicknames are set at issuance.
- Project Owners/system administrators can view every user's project tokens. Other users see only tokens they issued; Viewers remain read-only.
- Ownership of already issued tokens is unchanged.

### Installation and upgrade guidance

- README separately explains unauthenticated/token-authenticated client configuration, including Claude/Codex examples, token-file creation/storage and reconnection after token changes.
- Added public UPGRADE_PATH.md covering version routes, mandatory intermediate versions, backup and recovery.

## 0.3.0 (2026-09-13)

### Project and user management

- Added project list/details/sidebar and improved project creation through a dialog.
- System administrators can create accounts and manage enabled state, system role and passwords.
- Project creators become Owners; projects can have multiple Owners, Developers and Viewers. Viewers are read-only; users without membership are denied access.
- Token issuance specifies owner/nickname and reserves the name. Tokens/reservations expire if initial registration does not succeed within 30 minutes.

### Accounts and initial setup

- My profile allows display-name/email edits and password changes. Password changes require the current password and new-password confirmation, then end all existing sign-ins.
- Added initial administrator password confirmation. The initial password is set only on the setup page; no plaintext initial-password file is created.

### Data storage and distribution builds

- Moved server user/project/token/registration storage to embedded SQLite, running inside server executables/Docker without a separate DB service. Existing JSON state migrates once.
- Uses explicit bind mounts for Docker data, preserving DB/shared memory after container replacement.
- Unified client/server build options to reduce debugging information, local build paths and VCS metadata. Runtime version display remains; this does not remove all Go function names, types or strings.

## 0.2.1 (2026-09-13)

### Public distribution begins

- Client/server executables are downloadable without GitHub login through a separate public distribution repository.
- Added public Docker images. Publication verifies Linux AMD64/ARM64 images before making the Release public, then updates `latest` after publication completes.
- Added a standalone README to the distribution repository covering executable selection, initial setup, MCP connection and upgrades.
- Expanded macOS/Windows executable permission guidance and migration from old installers to separate executables.

Retains the eight separate client/server executables and per-file SHA-256 verification introduced in 0.2.0.
