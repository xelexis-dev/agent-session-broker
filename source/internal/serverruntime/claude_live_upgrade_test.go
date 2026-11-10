package serverruntime

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/broker"
)

// This stand-in has two roles: a real parent process and the agents roster CLI.
// All files and credentials are synthetic. No real Claude executable is invoked.
// The client calls agents; it does not directly consume these fixture files.
const upgradeClaudeSource = `package main
import("encoding/json";"io";"os";"os/exec";"path/filepath";"strconv")
type row struct { Kind string ` + "`json:\"kind\"`" + `; SessionID string ` + "`json:\"sessionId\"`" + `; Name string ` + "`json:\"name\"`" + `; Status string ` + "`json:\"status\"`" + `; PID int ` + "`json:\"pid\"`" + `; CWD string ` + "`json:\"cwd\"`" + ` }
type config struct { Binary, Endpoint, Token, AuthDir string }
func check(ok bool){if !ok{os.Exit(70)}}
func main(){
 home:=os.Getenv("HOME"); sid:=os.Getenv("CLAUDE_CODE_SESSION_ID")
 sessions:=filepath.Join(home,".claude","sessions")
 transcript:=filepath.Join(home,".claude","projects","synthetic",sid+".jsonl")
 if len(os.Args)==4 && os.Args[1]=="agents" && os.Args[2]=="--json" && os.Args[3]=="--all" {
  files,e:=filepath.Glob(filepath.Join(sessions,"*.json"));check(e==nil && len(files)==1)
  data,e:=os.ReadFile(files[0]);check(e==nil);var r row;check(json.Unmarshal(data,&r)==nil && r.SessionID==sid)
  data,e=os.ReadFile(transcript);check(e==nil);var meta struct{SessionID string ` + "`json:\"sessionId\"`" + `};check(json.Unmarshal(data,&meta)==nil && meta.SessionID==sid)
  f,e:=os.OpenFile(filepath.Join(home,"observations"),os.O_CREATE|os.O_WRONLY|os.O_APPEND,0600);check(e==nil);_,e=f.Write([]byte{1});check(e==nil);check(f.Close()==nil)
  check(json.NewEncoder(os.Stdout).Encode([]row{r})==nil);return
 }
 check(len(os.Args)==3 && os.Args[1]=="--synthetic-parent")
 data,e:=os.ReadFile(os.Args[2]);check(e==nil);var c config;check(json.Unmarshal(data,&c)==nil)
 cwd,e:=os.Getwd();check(e==nil)
 check(os.MkdirAll(sessions,0700)==nil);check(os.MkdirAll(filepath.Dir(transcript),0700)==nil)
 data,e=json.Marshal(row{Kind:"interactive",SessionID:sid,Name:"synthetic",Status:"idle",PID:os.Getpid(),CWD:cwd});check(e==nil)
 check(os.WriteFile(filepath.Join(sessions,strconv.Itoa(os.Getpid())+".json"),data,0600)==nil)
 data,e=json.Marshal(map[string]string{"sessionId":sid});check(e==nil);check(os.WriteFile(transcript,append(data,'\n'),0600)==nil)
 cmd:=exec.Command(c.Binary,"--broker-url",c.Endpoint,"--token",c.Token,"--auth-dir",c.AuthDir)
 cmd.Stdin,cmd.Stdout,cmd.Stderr=os.Stdin,os.Stdout,io.Discard
 if cmd.Run()!=nil{os.Exit(71)}
}`

// Socket paths must remain short on Darwin, independently of t.TempDir depth.
// Each connection is bounded and handled serially; cleanup joins the reader.
type upgradeInbox struct {
	path     string
	token    string
	frames   chan broker.DeliveredMessage
	failures chan struct{}
}

func newUpgradeInbox(t *testing.T) *upgradeInbox {
	t.Helper()
	dir := shortSocketDir(t, "asb-inbox-")
	inbox := &upgradeInbox{path: filepath.Join(dir, "i.sock"), token: "synthetic-inbox-only", frames: make(chan broker.DeliveredMessage, 32), failures: make(chan struct{}, 1)}
	listener, err := net.Listen("unix", inbox.path)
	matrixRequire(t, err == nil, "native inbox listen")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			ok := inbox.read(conn)
			_ = conn.Close()
			if !ok {
				select {
				case inbox.failures <- struct{}{}:
				default:
				}
			}
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); <-done })
	return inbox
}
func (i *upgradeInbox) read(conn net.Conn) bool {
	scanner := bufio.NewScanner(io.LimitReader(conn, 128<<10))
	scanner.Buffer(make([]byte, 4096), 64<<10)
	var auth struct{ Type, Token string }
	if !scanner.Scan() || json.Unmarshal(scanner.Bytes(), &auth) != nil || auth.Type != "auth" || auth.Token != i.token {
		return false
	}
	found := false
	for scanner.Scan() {
		var frame struct {
			Type, From, UUID, Priority string
			Message                    struct{ Role, Content string }
		}
		if json.Unmarshal(scanner.Bytes(), &frame) != nil || frame.Type != "user" || frame.From != "x-broker" || frame.Priority != "next" || frame.Message.Role != "user" || len(frame.UUID) != 36 {
			return false
		}
		var message broker.DeliveredMessage
		if json.Unmarshal([]byte(frame.Message.Content), &message) != nil {
			return false
		}
		select {
		case i.frames <- message:
		default:
			return false
		}
		found = true
	}
	return found && scanner.Err() == nil
}
func (i *upgradeInbox) expect(t *testing.T, from, to, body string) {
	t.Helper()
	select {
	case <-i.failures:
		t.Fatal("native inbox fixed-schema validation failed")
	case message := <-i.frames:
		matrixRequire(t, message.From == from && message.To == to && message.Body == body, "native inbox selected message fields")
	case <-time.After(60 * time.Second):
		t.Fatal("native inbox frame missing")
	}
}
func upgradeClaudeObservations(t *testing.T, home string) int {
	raw, err := os.ReadFile(filepath.Join(home, "observations"))
	matrixRequire(t, err == nil && len(raw) <= 65536, "bounded synthetic roster observations")
	return len(raw)
}
func upgradeClaudeStart(t *testing.T, binary, helper, endpoint, token, auth, home, cwd, sid string, inbox *upgradeInbox) (*matrixClient, *exec.Cmd) {
	config, _ := json.Marshal(matrixChildConfig{binary, endpoint, token, auth})
	path := filepath.Join(t.TempDir(), "child.json")
	matrixRequire(t, os.WriteFile(path, config, 0600) == nil, "private Claude child config")
	cmd := exec.Command(helper, "--synthetic-parent", path)
	cmd.Dir = cwd
	cmd.Stderr = io.Discard
	cmd.Env = []string{"HOME=" + home, "USERPROFILE=" + home, "PATH=" + filepath.Dir(helper) + string(os.PathListSeparator) + os.Getenv("PATH"), "TMPDIR=" + os.Getenv("TMPDIR"), "CLAUDE_CODE_SESSION_ID=" + sid, "CLAUDE_CODE_MESSAGING_TOKEN=" + inbox.token, "CLAUDE_CODE_MESSAGING_SOCKET=" + inbox.path}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "synthetic-Claude-upgrade", Version: "1"}, nil).Connect(t.Context(), &mcp.CommandTransport{Command: cmd, TerminateDuration: 3 * time.Second}, nil)
	matrixRequire(t, err == nil, "Claude stdio initialize")
	c := &matrixClient{session}
	t.Cleanup(c.close)
	return c, cmd
}
func TestPublicClaudeLiveServerUpgrade(t *testing.T) {
	assets := os.Getenv("ASB_UPGRADE_ASSETS")
	if assets == "" {
		t.Skip("opt-in: ASB_UPGRADE_HARNESS=claude scripts/tests/live_upgrade_recovery.sh")
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("public darwin-arm64 verification")
	}
	oldServer, newServer := upgradeBinary(t, assets, "0.14.0", "server"), upgradeBinary(t, assets, "0.15.0", "server")
	clients := map[string]string{}
	for _, v := range []string{"0.14.0", "0.15.0", "0.13.0"} {
		clients[v] = upgradeBinary(t, assets, v, "client")
	}
	// Establish that the installed global 0.14.0 is byte-identical when requested.
	if global := os.Getenv("ASB_UPGRADE_GLOBAL_CLIENT"); global != "" {
		a, e := os.ReadFile(global)
		matrixRequire(t, e == nil, "global client read")
		b, e := os.ReadFile(clients["0.14.0"])
		matrixRequire(t, e == nil && upgradeSHA(a) == upgradeSHA(b), "installed global release bytes")
		clients["0.14.0"] = global
		t.Log("CLAUDE_GLOBAL 0.14.0 release-bytes=true")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "main.go")
	helper := filepath.Join(dir, "claude")
	matrixRequire(t, os.WriteFile(source, []byte(upgradeClaudeSource), 0600) == nil, "synthetic Claude source")
	matrixRequire(t, exec.Command("go", "build", "-o", helper, source).Run() == nil, "synthetic Claude build")
	legacyBuilder := upgradeLegacyBuilder(t)
	for _, variant := range []string{"quick", "long", "during-call"} {
		if !t.Run(variant, func(t *testing.T) {
			upgradeVariant(t, oldServer, newServer, legacyBuilder, helper, clients, variant, "claude")
		}) {
			t.FailNow()
		}
	}
}
