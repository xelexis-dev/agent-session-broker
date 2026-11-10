## 연결된 채 업그레이드

**1.0 변경:** spawn 도구·발급 제거. v5 scope 원자 정리, 빈 목록은 무변경 시작 거부. 전체 응답은 2.0까지 호환용 `session:spawn` 유지(기능 없음). 롤백은 전체 백업 복원.

권장 client는 1.0.1(Node.js 22 이상)입니다. 서버·client 모두 상대 버전 0.17.3 이상이 필요하며 legacy·placeholder를 거부합니다. 모든 PC의 client를 먼저 갱신하고 MCP를 재연결한 뒤 서버를 교체하세요. 첨부는 서버와 송신·수신 client 모두 0.18.0 이상이 필요합니다.

`auth_status.update`는 연결한 서버가 알려 준 `recommendedClientVersion`보다 현재 `clientVersion`이 낮을 때만 표시됩니다. 서버 버전과 npm client 버전은 별개이며, 구서버가 권장값을 주지 않으면 안내를 표시하지 않습니다. 이 값은 인터넷의 latest가 아니라 서버 릴리스에서 게시 여부를 검증한 권장 버전입니다.

전역 설치 사용자는 `npm install -g @xelexis/x-broker@<권장 버전>`으로 갱신하고 `x-broker --version`을 확인한 뒤 아래 방법으로 재시작하거나 재연결하세요. npx 사용자는 MCP 설정의 고정 버전을 권장 버전으로 바꾸고 `npx -y @xelexis/x-broker@<권장 버전> --version`으로 준비합니다. 전역 설치 갱신만으로 별도의 npx-pin은 바뀌지 않습니다. `--version`은 버전 확인이며 대화 연결 갱신이 아닙니다.

- **Claude:** 설치 또는 설정 버전을 갱신한 뒤 해당 대화에서 `/mcp` → `x-broker` → `Reconnect`를 실행합니다. 도구 목록과 실제 `clientVersion`이 갱신됐는지 확인합니다.
- **Codex:** 원래 host 방식과 플래그를 유지합니다. `-c`로 연 TUI는 내장 app-server, `-c` 없는 resume은 공유 daemon을 사용합니다. 원래 플래그 그대로 종료 후 다시 열고, exec 대화는 exec 방식으로 resume합니다. 공유 daemon 기반 TUI를 다시 여는 것만으로 MCP가 새로 실행된다고 보장하지 않습니다.
- **Codex daemon reload:** `config/mcpServer/reload`는 격리 app-server에서 확인한 실험적 방법입니다. 공유 daemon에서는 다른 프로젝트를 포함한 모든 thread에 영향을 줄 수 있으므로 사용자가 영향 범위를 판단할 때만 선택합니다. x-broker와 PM은 이를 자동 실행하지 않습니다.
- **Windows:** 0.17.3 이상 npm launcher는 검증된 전용 실행파일 사본을 사용하므로 세션을 유지한 채 `npm install -g`로 갱신할 수 있습니다. 이전 launcher에서 처음 갱신할 때는 실행 중인 asb-client를 종료해야 합니다. 캐시를 쓸 수 없어 원본 실행으로 fallback했다는 stderr 안내가 있으면 그 세션도 업그레이드 전에 종료하세요. 실행 중 client 버전은 MCP 재연결 때 바뀝니다.
- **Kimi:** 업그레이드·재연결은 미측정입니다. 설치 또는 설정 버전을 갱신하고 Kimi를 재시작하세요.

daemon `restart`·`update`는 같은 daemon의 모든 대화 작업을 끊을 수 있습니다. daemon steer는 재시작 뒤 기록·대기열로 확인하며 미확인 메시지와 뒤의 전달을 보류합니다. embedded·Windows 전환과 기존 hook·queue의 짧은 저장 창에는 중복 가능성이 남습니다. 프로젝트에 맞춰 daemon(즉시 전달·재시작 중복 보호)과 embedded(대화별 환경·도구 경계 전달)를 선택하고, 한 대화에서는 모드를 유지하세요.

Codex는 도구 목록을 세션마다 고정하므로 새 client가 추가한 도구는 Codex 세션을 다시 열어야 보입니다. Claude는 `/mcp` Reconnect로 도구 목록을 다시 읽습니다.

재연결 뒤 `auth_status.clientVersion`, 같은 등록 ID·이름, `get_session.automaticDeliveryReady`와 실제 메시지 수신 재개를 확인합니다. 업그레이드하려고 등록을 retire하거나 새로 attach하지 않습니다.
