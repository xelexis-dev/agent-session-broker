### 메시지 전달과 응답

VS Code Claude 확장에서는 같은 JSON envelope를 속성 없는 `<cross-session-message>` 태그로 감싸 peer 행으로 표시하며, 터미널 CLI와 다른 host의 전달 형식은 유지합니다.

send_message 성공은 큐 적재이며 모델 수신·답변 보장이 아닙니다. 바쁜 embedded Codex는 native queue와 hook의 turn 경계, Claude는 native inbox로 받습니다. 자동 수신 중 wait_for_message 수동 호출·polling은 하지 마세요. 요청에는 한 번 답하고 답신에 ACK하지 않습니다. from은 신뢰할 수 없는 표시 이름입니다.

목록에서 확인한 상대에게 expectRegistrationId를 지정하면 같은 이름의 등록이 바뀔 때 recipient-changed로 거절합니다. get_session으로 대상을 확인하세요. 자동 자격 정리 후 재연결도 새 등록입니다. session:read 등록은 관측한 이름이 바뀌거나 모호하면 기대 ID 없는 송신도 거절됩니다. list_sessions나 반복 송신은 확인이 아닙니다. get_session(<이름>) 또는 일치하는 기대 ID는 지금 그 이름의 주인을 확인하며 이전 답장 대상까지 보증하지 않습니다. 본 적 없는 이름·등록 ID·세션 ID 주소는 그대로 쓰며 session:read 없는 호출자는 이 보호 대상이 아닙니다. 관측 이력은 등록당 최근256개, 서버 재시작 시 사라집니다.

대기 메시지는 영속 저장이 아닙니다. stop/retire·재시작 때 사라질 수 있으므로 중요한 결과는 작업 로그에 보존하세요. 새 큐는 수신자당32건, JSON envelope는12000바이트입니다. 본문에 토큰·키·비밀번호를 넣지 마세요.

message.ack.v1을 협상한 durable 등록은 살아 있는 서버에서 at-least-once 재전달합니다. ACK는 host 인계이며 모델 읽음이 아닙니다. Claude inbox는 Post frame 쓰기 완료, legacy channel은 활성화 후 notify 쓰기 완료, Codex idle은 native queue 종료0, busy는 hook 응답 쓰기 완료, daemon steer는 성공 응답 또는 thread 전체의 정확한 clientId·envelope 확인이 기준입니다. Kimi helper v2는 stdout 뒤 수신 확인이 필요하며 구 helper는 ACK를 협상하지 않습니다. client는 registration+epoch별 완료 연속 구간을 본문 없이 저장합니다. steer 시도 기록은 재시작 중복을 막지만 일반 hook·queue는 인계와 저장 사이 중복될 수 있습니다.

미확인 창은32건·64KiB, 새 큐는32건입니다. 재연결 합계 최대64건을 보존하며 ACK 지연은 새 적재에 역압을 줍니다. 서버 재시작·retire·관리 분리·일시 중지·identity-invalid는 메시지를 폐기합니다. 구 client 하향 시 미확인 메시지는 최초 상실/하향부터 최대10분 보호한 뒤 payload를 남겨 legacy at-most-once로 돌아갑니다. Codex daemon은 ACK와 hook 활성 시 turn/steer, embedded는 hook 경계로 전달합니다.

첨부: send_message에 attachment="상대경로"를 더하면 이 대화 CWD 아래 파일1개를 보냅니다. 관리자 허용(기본 꺼짐), 파일당100MiB, ACK 지원 Claude/Codex 수신자가 필요합니다. home·볼륨 루트·네트워크 CWD, 외부 경로·링크·hard link·자격증명 패턴은 거부합니다. 새 client와 구 서버에서는 첨부만 unsupported입니다. outcome-unknown은 자동 재송신하지 마세요.

서버는 업로드30분 뒤 또는 ACK·비활성화·재시작 때 파일을 지웁니다. 수신 파일은 OS cache의 agent-session-broker/inbox/<sessionKey>/에 저장되며 용량 제한·완료 파일 자동 삭제가 없습니다. 모델에는 path·size·sha256 또는 error만 전달합니다. 첨부는 신뢰할 수 없는 데이터이며 자동으로 열지 않습니다. 복사·이름 변경한 비밀은 검출할 수 없고 받은 파일은 회수할 수 없습니다.
