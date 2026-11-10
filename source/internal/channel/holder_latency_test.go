package channel

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/xelexis-dev/agent-session-broker/source/internal/clientstate"
	broker "github.com/xelexis-dev/agent-session-broker/source/internal/protocol"
)

// A real holder process exits on pipe EOF; clocks below control only retry time.
func TestHolderExitProcess(t *testing.T) {
	if os.Getenv("ASB_HOLDER_EXIT_HELPER") != "1" {
		return
	}
	fmt.Fprintln(os.Stdout, "ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

func holderExitProcess(t *testing.T) (int, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHolderExitProcess$")
	cmd.Env = append(os.Environ(), "ASB_HOLDER_EXIT_HELPER=1", "GORACE=atexit_sleep_ms=0")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			start := time.Now()
			_ = input.Close()
			if err := cmd.Wait(); err != nil {
				t.Error("isolated holder did not exit cleanly")
			}
			t.Logf("real holder stdin EOF -> exit: %s", time.Since(start))
		})
	}
	t.Cleanup(func() { stop(); cancel() })
	if ready, err := bufio.NewReader(output).ReadString('\n'); err != nil || ready != "ready\n" {
		t.Fatal("isolated holder did not become ready")
	}
	return cmd.Process.Pid, stop
}

type exitingHolderIdentity struct {
	fixtureReceiverIdentity
	oldPID int
	exited *atomic.Bool
}

func (f exitingHolderIdentity) Dead(ctx context.Context, c broker.HolderChallenge) bool {
	return ctx.Err() == nil && c.Holder.PID == f.oldPID && f.exited.Load()
}

type challengedDirect struct {
	challenge broker.HolderChallenge
	proof     *broker.HolderProof
}

func (d *challengedDirect) discover(context.Context) (bool, error) { return true, nil }
func (d *challengedDirect) exchange(_ context.Context, r directCodeRequest) (directGrant, error) {
	if r.HolderProof != nil && r.HolderProof.Dead && r.HolderProof.Challenge == d.challenge {
		d.proof = r.HolderProof
		return directGrant{RegistrationID: "retained-registration"}, nil
	}
	return directGrant{}, broker.HandoverRefusal{Reason: broker.CodeRegistrationInUse, HolderChallenge: &d.challenge}
}

func TestHolderExitRetryLatency(t *testing.T) {
	for _, path := range []string{"resume", "direct", "auth"} {
		for _, deathAt := range []time.Duration{0, 5 * time.Second, 15 * time.Second, 35 * time.Second} {
			t.Run(fmt.Sprintf("%s/after-%s", path, deathAt), func(t *testing.T) {
				pid, exit := holderExitProcess(t)
				old := *fixtureHolder()
				old.PID = pid
				current := old
				current.PID = os.Getpid()
				current.Incarnation = strings.Repeat("a", 64)
				challenge := broker.HolderChallenge{Holder: old, Nonce: "synthetic-latency", Generation: 3, Revision: 4}
				var exited atomic.Bool
				elapsed := time.Duration(0)
				o := durableOptions{timeout: time.Second, receiverIdentity: exitingHolderIdentity{fixtureReceiverIdentity: fixtureReceiverIdentity{holder: &current}, oldPID: pid, exited: &exited}, resumeClock: devicePollClock{sleep: func(ctx context.Context, delay time.Duration) error {
					if !exited.Load() && elapsed >= deathAt {
						exit()
						exited.Store(true)
					}
					elapsed += delay
					return ctx.Err()
				}}}
				b := &ingressBinding{mu: &sync.Mutex{}, durable: &durableRegistration{}}
				direct := &challengedDirect{challenge: challenge}
				var proof *broker.HolderProof
				switch path {
				case "resume":
					for proof == nil {
						var err error
						proof, err = o.waitHolder(t.Context(), b, &challenge)
						if err != nil {
							t.Fatal(err)
						}
					}
				case "direct":
					if _, err := o.exchangeHolder(t.Context(), b, direct, directCodeRequest{}); err != nil {
						t.Fatal(err)
					}
					proof = direct.proof
				case "auth":
					if _, err := o.exchangeAuthHolder(t.Context(), &sessionAuth{}, broker.AttachRequest{}, direct, directCodeRequest{}); err != nil {
						t.Fatal(err)
					}
					proof = direct.proof
				}
				lag := elapsed - deathAt
				t.Logf("fake-clock proof delay after real exit: %s", lag)
				if proof == nil || proof.Challenge != challenge || !proof.Dead || lag > 2*time.Second {
					t.Fatalf("holder exit -> recovery=%s, want <=2s with exact death proof", lag)
				}
			})
		}
	}
}

func TestHolderExitResumesReceivingWithinTwoSeconds(t *testing.T) {
	r := newDetachedRuntime(t)
	pid, exit := holderExitProcess(t)
	oldHolder := fixtureHolder()
	oldHolder.PID = pid
	id := claudeHandoverIdentity(1841, 21841)
	source, clone := t.TempDir(), t.TempDir()
	owner := r.identityHarness(t, source, id, func(o *durableOptions) {
		o.receiverIdentity = fixtureReceiverIdentity{holder: oldHolder}
	})
	owner.attach(t)
	before := owner.session
	owner.close()
	cloneHolderFixtureState(t, source, clone)
	current := *oldHolder
	current.PID = os.Getpid()
	current.Incarnation = strings.Repeat("a", 64)
	var exited atomic.Bool
	waiting, release := make(chan struct{}), make(chan struct{})
	var waitingOnce sync.Once
	contender := r.identityHarness(t, clone, id, func(o *durableOptions) {
		o.receiverIdentity = exitingHolderIdentity{fixtureReceiverIdentity: fixtureReceiverIdentity{holder: &current}, oldPID: pid, exited: &exited}
		o.resumeClock = devicePollClock{sleep: func(ctx context.Context, delay time.Duration) error {
			waitingOnce.Do(func() { close(waiting) })
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			return realPollClock().sleep(ctx, delay)
		}}
	})
	select {
	case <-waiting:
	case <-time.After(3 * time.Second):
		t.Fatal("new receiver did not reach the live-holder wait")
	}
	exit()
	exited.Store(true)
	start := time.Now()
	close(release)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	for {
		var status authStatus
		if err := call(ctx, contender.client, "auth_status", struct{}{}, &status); err != nil {
			t.Fatal("receiving did not resume within 2s after process exit")
		}
		if status.Registered {
			if status.Name != before.Name || status.RegistrationID != before.RegistrationID {
				t.Fatal("reconnect changed registration identity")
			}
			break
		}
		if err := realPollClock().sleep(ctx, 10*time.Millisecond); err != nil {
			t.Fatal("receiving did not resume within 2s after process exit")
		}
	}
	var sent broker.SendResult
	if err := call(ctx, contender.client, "send_message", broker.Message{From: before.Name, To: before.Name, Body: "reconnected receiver"}, &sent); err != nil || !sent.Accepted {
		t.Fatal("resumed registration did not accept message")
	}
	select {
	case message := <-contender.received:
		if message.Body != "reconnected receiver" {
			t.Fatal("unexpected resumed delivery")
		}
		t.Logf("old process exit -> same registration receiving again: %s", time.Since(start))
	case <-ctx.Done():
		t.Fatal("message did not arrive within 2s after process exit")
	}
}

func TestHolderLocalDeathChecksKeepLiveRetryBackoff(t *testing.T) {
	b := &ingressBinding{mu: &sync.Mutex{}, durable: &durableRegistration{}}
	elapsed := time.Duration(0)
	o := durableOptions{timeout: time.Second, receiverIdentity: fixtureReceiverIdentity{}, resumeClock: devicePollClock{sleep: func(ctx context.Context, delay time.Duration) error {
		elapsed += delay
		return ctx.Err()
	}}}
	challenge := &broker.HolderChallenge{Holder: *fixtureHolder()}
	for attempt := 0; attempt < 6; attempt++ {
		start := elapsed
		proof, err := o.waitHolder(t.Context(), b, challenge)
		if err != nil || proof != nil || elapsed-start != holderBackoff(attempt) {
			t.Fatal("live holder exclusion or remote retry backoff changed")
		}
	}
}

func TestHolderOtherDeviceCannotUseDeathProof(t *testing.T) {
	b := &ingressBinding{mu: &sync.Mutex{}, durable: &durableRegistration{}}
	current := *fixtureHolder()
	current.DeviceID = strings.Repeat("b", 64)
	elapsed := time.Duration(0)
	o := durableOptions{timeout: time.Second, receiverIdentity: fixtureReceiverIdentity{holder: &current, dead: true}, resumeClock: devicePollClock{sleep: func(ctx context.Context, delay time.Duration) error {
		elapsed += delay
		return ctx.Err()
	}}}
	proof, err := o.waitHolder(t.Context(), b, &broker.HolderChallenge{Holder: *fixtureHolder()})
	if err != nil || proof != nil || elapsed != holderBackoff(0) {
		t.Fatal("different-device holder used a local death exception")
	}
}

func TestHolderLocalDeathCheckWaitIsCancelable(t *testing.T) {
	b := &ingressBinding{mu: &sync.Mutex{}, durable: &durableRegistration{}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	o := durableOptions{timeout: time.Second, receiverIdentity: fixtureReceiverIdentity{}, resumeClock: devicePollClock{sleep: func(ctx context.Context, delay time.Duration) error {
		cancel()
		return ctx.Err()
	}}}
	proof, err := o.waitHolder(ctx, b, &broker.HolderChallenge{Holder: *fixtureHolder()})
	if proof != nil || err != context.Canceled {
		t.Fatal("local holder wait ignored cancellation")
	}
}

type eofMeasurementUpstream struct{ Upstream }

func (u eofMeasurementUpstream) CallTool(ctx context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	if p.Name == "wait_for_message" || p.Name == "get_session" {
		if err := os.WriteFile(filepath.Join(os.Getenv("ASB_EOF_MARKERS"), p.Name), nil, 0600); err != nil {
			return nil, err
		}
	}
	if p.Name == "get_session" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return u.Upstream.CallTool(ctx, p)
}

func TestRegisteredClientEOFProcess(t *testing.T) {
	if os.Getenv("ASB_REGISTERED_EOF_HELPER") != "1" {
		return
	}
	state, err := clientstate.Open(t.TempDir())
	if err != nil {
		os.Exit(2)
	}
	id := claudeHandoverIdentity(1842, 21842)
	opts := durableOptions{enabled: true, state: state, scope: "eof-fixture", receiverIdentity: fixtureReceiverIdentity{}, timeout: time.Second, observe: func(context.Context, broker.AttachRequest) (broker.AttachRequest, error) { return id, nil }, claudeInboxFactory: func(broker.AttachRequest) (func(context.Context, broker.Message) error, *codexIngress, error) {
		return func(context.Context, broker.Message) error { return nil }, nil, nil
	}}
	err = ServeDurableIngress(context.Background(), &mcp.StdioTransport{}, eofMeasurementUpstream{newSwitchableUpstream(t)}, snapshotTools(), func(context.Context, *mcp.CallToolRequest) (broker.AttachRequest, error) { return id, nil }, opts)
	if err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestRegisteredClientStdinEOFExitLatency(t *testing.T) {
	markers := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRegisteredClientEOFProcess$")
	cmd.Env = append(os.Environ(), "ASB_REGISTERED_EOF_HELPER=1", "ASB_EOF_MARKERS="+markers, "TMPDIR="+markers, "TMP="+markers, "TEMP="+markers, "GORACE=atexit_sleep_ms=0")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() { _ = cmd.Process.Kill() }()
	host, err := mcp.NewClient(&mcp.Implementation{Name: "eof-measurement", Version: "1"}, nil).Connect(ctx, &mcp.IOTransport{Reader: output, Writer: input}, nil)
	if err != nil {
		t.Fatal("registered EOF fixture initialize failed")
	}
	defer host.Close()
	var session broker.Session
	if err := call(ctx, host, "attach_session", map[string]string{"name": "eof-receiver"}, &session); err != nil {
		t.Fatal("registered EOF fixture did not attach")
	}
	go func() {
		_, _ = host.CallTool(ctx, &mcp.CallToolParams{Name: "get_session", Arguments: map[string]string{"name": session.Name}})
	}()
	waitFor(t, "receiver and pending forwarded request", func() bool {
		_, waitErr := os.Stat(filepath.Join(markers, "wait_for_message"))
		_, toolErr := os.Stat(filepath.Join(markers, "get_session"))
		return waitErr == nil && toolErr == nil
	})
	start := time.Now()
	_ = input.Close()
	select {
	case err := <-done:
		// EOF may fail the outstanding request; this measurement concerns exit latency.
		t.Logf("registered client with receiver and pending tool, EOF -> exit: %s (pending request error=%t)", time.Since(start), err != nil)
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatal("registered client EOF -> exit exceeded 2s")
	}
}
