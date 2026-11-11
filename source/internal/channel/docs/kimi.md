### Kimi Code (macOS·Linux, turn-boundary 수신)

Kimi는 프로젝트 루트의 `.mcp.json`을 읽습니다. 위 Claude 예제와 같은 형식을 쓰되 **서버와 클라이언트 모두 0.17.2 이상**이 필요합니다. 이전 서버에 연결하면 등록을 시도하기 전에 `unsupported-server`로 안내합니다. Windows Kimi는 지원하지 않습니다.

Kimi 홈 `~/.kimi-code/config.toml`(또는 `$KIMI_CODE_HOME/config.toml`)에 아래 hook을 설정하세요. 자동으로 전역 설정을 바꾸지 않습니다. hook 명령에 broker 토큰을 넣지 마세요.

MCP 설정과 같은 고정 버전을 `npm install -g @xelexis/x-broker@1.0.4`로 설치하고, `command -v x-broker`가 출력한 절대 경로를 아래 hook 명령에 사용하세요. hook의 `PATH`에서 `node`를 찾을 수 있어야 하며, 업그레이드할 때도 전역 설치 버전을 MCP 설정과 같게 유지하세요.

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

MCP client의 `--auth-dir`/`--state-dir`와 hook의 경로를 같게 지정하세요. 둘 다 생략하면 동일한 `AGENT_SESSION_BROKER_STATE_DIR` 환경변수와 사용자 기본 경로를 사용합니다. 경로가 다르면 hook이 소켓을 찾지 못해 `kimi-session-unknown`이 됩니다. 긴 상태 경로는 소유권을 확인한 짧은 사용자 전용 디렉터리로 소켓만 옮깁니다. 소켓 bind 실패는 `auth_status`에 `kimi-hook-unavailable`로 표시됩니다.

메시지는 **다음 eligible UserPromptSubmit 또는 Stop hook**에서 전달합니다. idle·실패·이미 Stop으로 이어진 턴은 다음 사용자 입력까지 기다릴 수 있습니다. idle 대화를 깨우지 않습니다. `auth_status`의 `delivery: "turn-boundary"`와 `pendingMessages`를 확인하세요. 공개 세션 목록의 `automaticDeliveryReady: false`는 이 제한을 뜻하며, 큐 접수를 막지 않습니다. 활동은 `unknown / unsupported`입니다.

Kimi 2.3.1은 한 턴의 Stop continuation을 한 번만 실행합니다. 그 뒤 도착한 메시지는 다음 사용자 입력까지 남습니다. client가 batch를 hook으로 넘긴 뒤 Esc·timeout 등으로 hook이 중단되면 그 batch가 유실될 수 있으며, `kimi-hook-handoff` 진단은 handoff만 기록하고 모델 수신을 증명하지 않습니다. 대화가 바뀌면 기존 수신을 차단하고 `reconnect-required`를 표시합니다. 새 대화로 MCP를 재연결한 뒤 명시적으로 연결을 요청하세요.
