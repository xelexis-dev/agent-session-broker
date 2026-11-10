package channel

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

func TestOptimization2EnvelopeMeasure(t *testing.T) {
	if os.Getenv("ASB_OPT2_ENVELOPE") != "1" {
		t.Skip("opt-in synthetic envelope bytes")
	}
	message := broker.Message{DeliverySequence: 1, FromRegistrationID: strings.Repeat("X", 26), From: "worker-abc-1", To: "planner", Body: strings.Repeat("x", 100), SentAt: time.Date(2026, 10, 31, 1, 2, 3, 0, time.UTC)}
	codex, e := codexMessageContent([]broker.Message{message})
	if e != nil {
		t.Fatal(e)
	}
	claude, e := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/claude/channel", "params": map[string]any{"content": message.Body, "meta": messageMetadata(message)}})
	if e != nil {
		t.Fatal(e)
	}
	for name, text := range map[string]string{"codex-steer-prefix": codex, "claude-channel-wire": string(claude)} {
		if len(text) <= 100 {
			t.Fatal("missing envelope")
		}
		fmt.Printf("OPT2_ENVELOPE %s body=100 total=%d overhead=%d estimatedTokens=%.2f\n", name, len(text), len(text)-100, float64(len(text))/4)
	}
}
