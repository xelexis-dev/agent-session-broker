package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
)

type versionReplyBackend struct {
	credentialScopeBackend
	err error
}

func (b *versionReplyBackend) result() broker.RegisterResult {
	v := b.credentialScopeBackend.result()
	v.ServerVersion = "forged-backend-version"
	v.Session.RegistrationID = "fixture-registration"
	return v
}
func (b *versionReplyBackend) RegisterSession(context.Context, broker.RegisterRequest) (broker.RegisterResult, error) {
	return b.result(), b.err
}
func (b *versionReplyBackend) ResumeSession(context.Context, broker.ResumeRequest) (broker.RegisterResult, error) {
	return b.result(), b.err
}
func (b *versionReplyBackend) RenewLease(context.Context, broker.RenewRequest) (broker.RegisterResult, error) {
	return b.result(), b.err
}

func TestServerVersionRegistrationReplies(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		err        error
	}{{"success", "", nil}, {"refusal", broker.CodeOwned, nil}, {"error", "", errors.New("fixture failure")}} {
		t.Run(tc.name, func(t *testing.T) {
			client := catalogSession(t, New(&versionReplyBackend{credentialScopeBackend: credentialScopeBackend{code: tc.code}, err: tc.err}, &stubMemory{}, "1.1.0"))
			for _, tool := range []struct {
				name string
				args any
			}{{"register_session", broker.RegisterRequest{}}, {"resume_session", broker.ResumeRequest{}}, {"renew_lease", broker.RenewRequest{}}} {
				result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: tool.name, Arguments: tool.args})
				if err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(result.StructuredContent)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]any
				if json.Unmarshal(raw, &fields) != nil {
					t.Fatal("malformed result")
				}
				if tc.code == "" && tc.err == nil {
					if fields["serverVersion"] != "1.1.0" {
						t.Errorf("%s lacks actual server version", tool.name)
					}
					// Released clients use json.Unmarshal: an additional optional field must leave their projection intact.
					var old struct {
						Code             string         `json:"code"`
						Session          broker.Session `json:"session"`
						CredentialScopes []string       `json:"credentialScopes,omitempty"`
					}
					if json.Unmarshal(raw, &old) != nil || old.Code != "" || old.Session.RegistrationID != "fixture-registration" {
						t.Errorf("%s broke old result decoder", tool.name)
					}
				} else if _, present := fields["serverVersion"]; present {
					t.Errorf("%s failure carries a server version", tool.name)
				}
			}
		})
	}
	client := catalogSession(t, New(&versionReplyBackend{}, &stubMemory{}, "1.1.0"))
	listed, err := client.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, tool := range listed.Tools {
		if tool.Name != "register_session" && tool.Name != "resume_session" && tool.Name != "renew_lease" {
			continue
		}
		raw, _ := json.Marshal(tool.OutputSchema)
		var schema struct {
			Properties map[string]struct{ Type any }
			Required   []string
		}
		if json.Unmarshal(raw, &schema) != nil || schema.Properties["serverVersion"].Type != "string" {
			t.Fatalf("%s output schema lacks version", tool.Name)
		}
		for _, required := range schema.Required {
			if required == "serverVersion" {
				t.Errorf("%s version must be optional", tool.Name)
			}
		}
		checked++
	}
	if checked != 3 {
		t.Fatalf("checked %d version output schemas", checked)
	}
}
