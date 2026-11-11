### 설치와 MCP 설정

Node.js 22 이상과 npm이 필요합니다. Windows x64, macOS ARM64·x64, Linux x64를 지원합니다. 기본은 게시된 버전을 고정한 전역 설치입니다.

```sh
npm view @xelexis/x-broker version
npm install -g @xelexis/x-broker@1.0.4
x-broker --version
```

예제는 `1.0.4`을 고정합니다. 서버만 바뀐 릴리스는 npm 게시를 생략하므로 서버와 클라이언트 버전이 다를 수 있습니다. 다른 버전은 게시 여부를 확인하고 설치 버전을 바꾸세요. `--version`은 버전 확인이며 대화 연결 검사가 아닙니다.

대안으로 `npx -y @xelexis/x-broker@1.0.4`을 쓸 수 있지만 세션마다 약 100 MB의 npm 프로세스 하나가 계속 남습니다. 이때 아래 `command`를 `npx`로 바꾸고 `args` 앞에 `"-y", "@xelexis/x-broker@1.0.4"`을 추가하세요. `@latest`는 시작 때 레지스트리 확인과 다운로드로 늦어질 수 있으므로 버전을 고정하세요.

macOS·Linux에서는 execve를 지원하는 Node의 전역 wrapper가 client로 교체됩니다. **Windows는 execve가 없어 어느 설치 방식이든 node relay가 남으며, 전역 설치는 npm exec 부모 프로세스를 제거합니다.**

## 3. MCP 설정

기존 설정이 있으면 `x-broker` 항목만 추가하거나 교체하세요. 서버 주소, `project=brk`, `발급받은_토큰`을 실제 값으로 바꾸세요. 토큰이 있는 파일은 Git이나 공유 폴더에 올리지 마세요.

MCP `args`에 `"--tool-profile", "worker"`를 추가하면 모든 하네스에서 관리·버그 조회 도구 7개를 목록에서 숨기고 해당 호출을 `tool-not-in-profile`로 거부합니다. 기본은 `full`이며 서버 권한이나 보안 경계를 바꾸지 않습니다.

**Claude Code — `.mcp.json`**

```json
{
  "mcpServers": {
    "x-broker": {
      "command": "x-broker",
      "args": [
        "--broker-url", "https://broker.example/mcp?project=brk",
        "--token", "발급받은_토큰"
      ]
    }
  }
}
```

**Codex — `.codex/config.toml`**

```toml
[mcp_servers.x-broker]
command = "x-broker"
args = [
  "--broker-url", "https://broker.example/mcp?project=brk",
  "--token", "발급받은_토큰"
]
```

Codex는 신뢰한 프로젝트의 설정만 읽습니다. 같은 OS 사용자와 인증 상태 디렉터리를 유지하세요. MCP 재연결 후 에이전트에게 이 대화를 연결해 달라고 요청할 때만 `attach_session`을 호출합니다. 기존 등록은 자동 복구합니다. `auth_status`로 등록 이름·clientVersion·serverVersion을 확인하세요. 승인 코드가 나오면 관리 화면에서 승인합니다. 상세 재연결은 `xbroker_docs topic=upgrade`, 연결 오류는 `topic=errors`를 읽으세요.
프로젝트마다 entry를 추가하는 방법은 [여러 프로젝트에 연결하기](#여러-프로젝트에-연결하기)를 보세요. 사용자 지정 `CODEX_HOME`은 각 MCP entry의 `env`에도 같은 경로로 지정합니다.
