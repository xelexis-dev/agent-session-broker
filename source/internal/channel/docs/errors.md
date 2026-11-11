## Error codes and actions

Before repeating a failed request, check current state with auth_status and list_sessions. Do not paste secrets or raw server responses into reports.

| Tool/code | Action |
| --- | --- |
| attach: name-in-use / identity-conflict | Choose another name or request cleanup of the existing registration. |
| attach: lease-conflict | Reconnect this conversation's MCP. |
| attach: invalid-identity / inspection-unavailable / unsupported-harness | Check that MCP belongs to the intended live conversation and inspect the local harness installation. |
| attach: legacy-disabled / capacity | Connect through a local MCP client or ask an administrator to clean old registrations. |
| send: recipient-changed | The name holder changed. Recheck registrationId in the list. |
| rename: nickname-in-use | For live-session/inactive-registration/detached-registration, verify registration; if needed, another session retires its ID. For orphaned-credential, consider name retire. For unused-credential, use the panel or expiry. Old servers without a reason show general guidance and server gave no reason. |
| retire: invalid-selector / name-held-by-registration | Specify only registrationId or name. For a registered target, use the exact ID. |
| retire: self-retire-denied / target-suspended / forbidden | Ask another session or the panel to retire you. Check suspension and management permissions. |
| retire: credential-unused | Clear unused credentials in the panel or by expiry. |
| retire: storage / cleanup-required | storage failed before saving detach; cleanup-required is unfinished after detach. Complete it with the same response registrationId. |
| retire: not-found | The previous call may have completed. Check the list and rename before retrying a name. |
| registration-not-found / reconnect-required | Another session may have retired this registration. When the user wants connection, reconnect MCP and explicitly attach. |
| insufficient_scope / server-incompatible / client-incompatible | Check required token permissions and supported versions. Read xbroker_docs topic upgrade. |

Check mode/reason in `auth_status.codexDelivery`. `steer-unchecked` is normal before first host verification. `unverified-host` means verification failed, except the Windows stub always uses hooks without verification. POSIX embedded changes from `hook/steer-unchecked` to `hook/unverified-host` after failure; `mode=hook` alone does not mean daemon is unsupported. `steer-uncertain`/`steer-recovery-pending` verifies handoff and holds later FIFO messages. `steer-recovery-unavailable` means attempt records cannot be read/written safely. Do not delete records or resend without confirmation. Restart/same-registration lease replacement preserves the hold; registration/epoch replacement retires old attempts. `--codex-steer off` disables only new steer. See [Codex execution modes](#codex-execution-modes) for protection and transition limits.

An idle conversation restarted without the launcher waits for its first tools/call carrying threadId (such as `auth_status`) before recovery queries. `steer-recovery-ambiguous` means content with the same clientId could not be verified and remains pending.

Name retire targets orphaned credentials at call time. Do not blindly retry the same name after timeout.
After a Claude automatic update on Windows, reconnect MCP with the latest client if `unsupported-harness` appears. If it persists, inspect safe diagnostics in `auth_status.parentDiagnostic`.
