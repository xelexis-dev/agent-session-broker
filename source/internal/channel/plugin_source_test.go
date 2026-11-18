package channel

import (
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"os"
	"testing"
)

func TestPluginSourceVariableClearedAndReported(t *testing.T) {
	for _, source := range []string{"userConfig", "directory", "invalid", "directory:/private/path", ""} {
		t.Setenv("AGENT_SESSION_BROKER_PLUGIN_SOURCE", source)
		t.Setenv(envToken, fixtureToken("asb_p_"))
		options, err := parseClientOptions([]string{"--broker-url", "https://broker.example/mcp?project=p1"})
		if err != nil {
			t.Fatal("fixture options")
		}
		if _, present := os.LookupEnv("AGENT_SESSION_BROKER_PLUGIN_SOURCE"); present {
			t.Fatal("plugin source environment retained before child startup")
		}
		expected := source
		if source != "userConfig" && source != "directory" {
			expected = ""
		}
		ctx := context.WithValue(t.Context(), clientConfigKey{}, clientConfig{source: options.configSource, broker: brokerScope(options.endpoint, options.project)})
		raw, _ := json.Marshal(authVersions(ctx, nil, authStatus{}))
		var out map[string]any
		if json.Unmarshal(raw, &out) != nil {
			t.Fatal("status JSON")
		}
		if expected == "" {
			if _, present := out["configSource"]; present {
				t.Fatal("invalid/absent source reported")
			}
		} else if out["configSource"] != expected {
			t.Fatal("plugin source not reported")
		}
		if out["broker"] != "https://broker.example/mcp?project=p1" {
			t.Fatal("broker diagnostic missing")
		}

	}
}

func TestPluginSourceClientWiring(t *testing.T) {
	t.Setenv("AGENT_SESSION_BROKER_PLUGIN_SOURCE", "directory")
	t.Setenv(envToken, fixtureToken("asb_p_"))
	client := startupClient(t, []string{"--broker-url", "http://127.0.0.1:1/mcp?project=p1&unused=synthetic-hidden-query", "--state-dir", t.TempDir()}, duplicateParent(t))
	result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "auth_status", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal("auth status wiring")
	}
	data, _ := json.Marshal(result.StructuredContent)
	var status map[string]any
	if json.Unmarshal(data, &status) != nil || status["configSource"] != "directory" || status["broker"] != "http://127.0.0.1:1/mcp?project=p1" {
		t.Fatal("client config provenance was not wired or broker query leaked")
	}
	if _, exists := os.LookupEnv("AGENT_SESSION_BROKER_PLUGIN_SOURCE"); exists {
		t.Fatal("client retained source env")
	}
}
