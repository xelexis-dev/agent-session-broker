## 오류 코드와 조치

오류 뒤 같은 요청을 반복하기 전에 auth_status와 list_sessions로 현재 상태를 확인하세요. 비밀값이나 서버 원문을 보고서에 붙이지 마세요.

| 도구·코드 | 조치 |
| --- | --- |
| attach: name-in-use / identity-conflict | 다른 이름을 고르거나 기존 등록 정리를 요청합니다. |
| attach: lease-conflict | 이 대화의 MCP를 재연결합니다. |
| attach: invalid-identity / inspection-unavailable / unsupported-harness | 의도한 살아 있는 대화의 MCP인지와 로컬 하네스 설치를 확인합니다. |
| attach: legacy-disabled / capacity | 로컬 MCP client로 연결하거나 관리자에게 오래된 등록 정리를 요청합니다. |
| send: recipient-changed | 이름 보유자가 바뀌었습니다. 목록의 registrationId를 다시 확인합니다. |
| rename: nickname-in-use | holder가 live-session·inactive-registration·detached-registration이면 등록을 확인하고 필요할 때 다른 세션이 ID로 retire합니다. orphaned-credential이면 name retire를 검토합니다. unused-credential이면 관리 화면 또는 만료로 정리합니다. 구서버가 사유를 주지 않으면 일반 안내와 server gave no reason을 표시합니다. |
| retire: invalid-selector / name-held-by-registration | registrationId 또는 name 하나만 지정합니다. 등록이 있으면 정확한 ID를 사용합니다. |
| retire: self-retire-denied / target-suspended / forbidden | 자신의 retire는 다른 세션이나 패널에 요청합니다. 일시 중지 해제와 관리 권한을 확인합니다. |
| retire: credential-unused | 미사용 자격은 관리 화면 또는 만료로 정리합니다. |
| retire: storage / cleanup-required | storage는 detach 저장 전 실패입니다. cleanup-required는 연결 해제 후 미완료이므로 응답의 같은 registrationId로 마무리합니다. |
| retire: not-found | 이전 호출이 끝났을 수 있습니다. name 재시도 전에 목록과 rename으로 확인합니다. |
| registration-not-found / reconnect-required | 다른 세션이 retire했을 수 있습니다. 사용자가 연결을 원할 때 MCP 재연결·명시적 attach를 요청합니다. |
| insufficient_scope / server-incompatible / client-incompatible | 필요한 토큰 권한 또는 지원 버전을 확인합니다. 절차는 xbroker_docs의 upgrade 주제를 읽습니다. |

`auth_status.codexDelivery`의 mode/reason으로 전달 경로를 확인합니다. `steer-unchecked`는 첫 호스트 검증 전의 정상 상태입니다. `unverified-host`는 검증 실패이며, Windows stub에서는 검증 없이 항상 hook을 쓴다는 예외입니다. POSIX embedded는 `hook/steer-unchecked`에서 검증 실패 후 `hook/unverified-host`로 바뀝니다. `mode=hook`만으로 daemon 미지원이라 판단하지 마세요. `steer-uncertain`·`steer-recovery-pending`은 전송 인계 확인 중이며 FIFO 뒤도 보류합니다. `steer-recovery-unavailable`은 시도 기록을 안전하게 읽거나 쓸 수 없는 상태입니다. 기록을 지우거나 확인 없이 다시 보내지 마세요. 재시작·같은 등록의 lease 교체도 보류를 유지하고, 등록·epoch 교체는 옛 시도를 은퇴시킵니다. `--codex-steer off`는 새 steer만 끕니다. 보호 범위와 모드 전환의 한계는 [Codex 실행 방식](#codex-실행-방식)을 보세요.

실행기 없이 재시작한 idle 대화는 threadId가 담긴 첫 tools/call(예: `auth_status`) 전까지 복구 조회를 기다립니다. 진단의 `steer-recovery-ambiguous`는 같은 clientId의 내용을 검증하지 못한 보류입니다.

이름 retire는 호출 시점의 고아 자격을 대상으로 합니다. 시간 초과 후 같은 이름에 무조건 재시도하지 마세요.
Windows에서 Claude 자동 업데이트 뒤 `unsupported-harness`가 나면 최신 client로 MCP를 다시 연결하고, 계속되면 `auth_status.parentDiagnostic`의 안전 진단을 확인하세요.
