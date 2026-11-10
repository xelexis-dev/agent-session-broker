### 세션 관리 권한

서버 0.17.1부터 `session:manage`로 자기 연결 해제와 소유권이 허용하는 다른 세션의 관리가 가능합니다. 기존 토큰도 같습니다.

일시 중지 등록은 제외하며, 마지막 통신 후 30일과 probe 기한을 모두 넘은 등록은 자동 분리한 뒤 약 70분 후 정리합니다. 재시작 유예와 순차 처리로 지연될 수 있고 이름·자격 해제는 조건부입니다.

- Owner 토큰은 같은 프로젝트의 모든 세션을 관리합니다. Developer 토큰은 같은 소유자의 세션만 관리합니다.
- 이 세션 자신은 `stop_session`으로 연결 해제만 할 수 있습니다. 자신의 `suspend_session`·`unsuspend_session` 호출은 허용되지 않습니다. 소유자 정보가 없는 기존 토큰도 자기 연결 해제만 할 수 있습니다.
- `stop_session`은 대상 프로세스를 종료하지 않고 연결을 해제하며 대기 메시지를 비웁니다. 재승인 가능한 현재 holder의 자동 자격은 행 정리 때 회수하며, 나머지 자격은 만료 또는 `retire_session`까지 이름을 예약합니다. 대상 대화의 사용자가 다시 연결을 요청할 때만 등록됩니다.
- `suspend_session`은 관리할 수 있는 다른 세션의 연결을 일시 중지하고 대기 메시지를 버리며 진행 중 요청을 취소합니다. `unsuspend_session`은 그 세션을 재개하며 대상은 다음 lease부터 다시 연결됩니다.
- 다른 프로젝트의 세션은 관리할 수 없습니다. 권한은 매 호출 시 현재 역할·소유권·토큰 상태로 다시 확인하므로 역할 강등·멤버 제외·토큰 회수도 반영됩니다.

`retire_session`은 클라이언트와 서버 모두 0.17.3 이상에서 지원합니다. 클라이언트는 고정된 도구 목록을 사용하므로 구 client를 재연결하는 것만으로 새 도구가 추가되지 않습니다.

`stop_session`은 다시 attach할 수 있는 연결 해제입니다. `retire_session`은 다른 세션의 등록과 연결 이력이 있는 세션 자격을 회수하여 이름 예약을 해제합니다. 프로세스를 종료하지 않으며 프로젝트 토큰과 연결하지 않은 대체 자격은 보존합니다. 자기 자신과 일시 중지된 대상은 retire할 수 없습니다.

| 도구 | 동작 |
| --- | --- |
| `xbroker_docs {topic?}` | client 0.17.3의 로컬 읽기 전용 도움말(서버 업데이트 불필요). 인증·등록·서버 호출 없이 목차 또는 주제 본문과 clientVersion을 반환합니다. |
| `stop_session {name}` | 연결과 대기 메시지를 정리합니다. 재승인 가능한 현재 holder의 자동 자격은 기록 제거 직전에 회수하며, 나머지는 이름 예약을 유지합니다. |
| `retire_session {registrationId}` | 정확한 등록과 그 등록에 바인딩된 세션 자격을 회수합니다. |
| `retire_session {name}` | 등록 없이 이름을 예약한 자격 중 과거 연결 이력이 있는 자격만 회수합니다. |

`registrationId`와 `name` 중 정확히 하나를 지정합니다. 이름을 보유한 등록이 있으면 ID를 확인해 호출하세요. 미사용 자격은 관리 화면 또는 만료로 정리합니다. `cleanup-required`는 연결 해제 뒤 저장이 끝나지 않은 상태이며 응답의 같은 `registrationId`로 재호출합니다. name 모드는 **호출 시점**의 고아 자격을 대상으로 하므로 결과가 불명하면 바로 재시도하지 말고 `list_sessions`와 rename으로 먼저 확인하세요. 성공 결과의 `revokedCredentials`에는 회수한 자격의 비밀이 아닌 ID와 첫 연결 시각이 표시됩니다.

rename_session의 nickname-in-use는 holder에 따라 live-session·inactive-registration·detached-registration(등록 확인 후 ID retire), orphaned-credential(name retire), unused-credential(패널 또는 만료)로 안내합니다. 구서버가 사유를 주지 않으면 server gave no reason을 표시합니다.
