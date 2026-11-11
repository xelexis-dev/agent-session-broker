### Session management permissions

From server 0.17.1, `session:manage` allows self-disconnection and management of other sessions within ownership limits, including with existing tokens.

Unsuspended registrations are automatically detached after both 30 days without contact and the probe deadline, then cleaned roughly 70 minutes later. Restart grace and sequential processing may delay this; name and credential release are conditional.

- Owner tokens manage all sessions in the same project. Developer tokens manage only sessions with the same owner.
- A session can only disconnect itself with `stop_session`; self-`suspend_session` and self-`unsuspend_session` are forbidden. Legacy tokens without owner information also allow only self-disconnection.
- `stop_session` disconnects without terminating the process and clears pending messages. A current holder's automatic credential that supports reapproval is revoked during row cleanup; other credentials reserve the name until expiry or `retire_session`. Registration resumes only when the target conversation's user requests connection.
- `suspend_session` suspends another manageable session, discards pending messages and cancels active requests. `unsuspend_session` resumes it; the target reconnects from the next lease.
- Cross-project management is forbidden. Each call rechecks current role, ownership and token state, including demotion, membership removal and token revocation.

`retire_session` requires both client and server 0.17.3 or later. Clients use a fixed tool list: reconnecting an old client does not add new tools.

`stop_session` disconnects with later attach possible. `retire_session` revokes another session's registration and previously connected session credentials, releasing name reservations. It does not terminate the process; project tokens and unconnected replacement credentials remain. Self-retire and retiring suspended targets are forbidden.

| Tool | Behavior |
| --- | --- |
| `xbroker_docs {topic?}` | Local read-only help in client 0.17.3; no server update needed. Returns the index or topic and clientVersion without authentication, registration or server calls. |
| `stop_session {name}` | Clears connection and pending messages. A current holder's reapprovable automatic credential is revoked just before record removal; other credentials keep reserving the name. |
| `retire_session {registrationId}` | Revokes the exact registration and session credentials bound to it. |
| `retire_session {name}` | Revokes only previously connected credentials reserving a name without a registration. |

Specify exactly one of `registrationId` or `name`. If a registration holds the name, inspect its ID and use it. Clear unused credentials in the panel or by expiry. `cleanup-required` means disconnection happened but storage is unfinished; retry with the same response `registrationId`. Name mode targets orphaned credentials **at call time**: after an uncertain result, check `list_sessions` and rename before retrying. Successful `revokedCredentials` contains non-secret credential IDs and first connection times.

rename_session's nickname-in-use identifies live-session, inactive-registration or detached-registration (verify registration, then retire by ID), orphaned-credential (name retire), or unused-credential (panel or expiry). If an old server omits the reason, it shows server gave no reason.
