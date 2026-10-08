// Package plugintest runs a stand-in MTProto agent on loopback for the plugin's
// tests: a real link endpoint behind REALITY, answering calls from memory.
package plugintest

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
	"github.com/nasnet-community/mtproto-remote-plugin/installer"
	"github.com/nasnet-community/mtproto-remote-plugin/link"
	"github.com/nasnet-community/mtproto-remote-plugin/reality"
	"github.com/nasnet-community/mtproto-remote-plugin/realityserver"
)

// Domains are the cover sites the fake accepts as SNI.
var Domains = []string{"cover.example.com", "other.example.com"}

type Agent struct {
	Mu         sync.Mutex
	States     []ctrl.State
	Applied    string
	CheckOK    bool
	Checked    []string
	Users      []ctrl.UserStat
	Running    bool
	CrashLoop  bool
	Held       string // why telemt is kept stopped
	Restarts   int
	Updates    []ctrl.UpdateRequest
	Uninstalls int
	Logs       []string
	CheckErr   string // when set, cover checks fail outright: no result at all
	// Gate runs before a call is answered; a test blocks in it to hold the call open.
	Gate func(method string, body json.RawMessage)

	AgentID string
	Port    int
	Pub     []byte
	ShortID [8]byte
	Token   []byte

	ag    *link.Agent
	dest  string
	priv  []byte
	names []string // the SNI it answers to; nil is Domains
}

// Start runs a warm agent until the test ends.
// OldTelemt is the proxy the fake agent runs: one behind the pin, so an update is due.
const OldTelemt = "3.4.24"

func Start(t testing.TB) *Agent {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	dest := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	dest.EnableHTTP2 = true
	dest.Config.ErrorLog = log.New(io.Discard, "", 0) // REALITY's probes of it are expected
	dest.StartTLS()
	t.Cleanup(dest.Close)
	priv, pub, err := reality.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{AgentID: "agent-test", Pub: pub, Token: random(32), Running: true, CheckOK: true, dest: dest.Listener.Addr().String(), priv: priv}
	copy(a.ShortID[:], random(8))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a.Port = ln.Addr().(*net.TCPAddr).Port
	a.ag = link.NewAgent(link.AgentConfig{
		AgentID: a.AgentID, Tokens: [][]byte{a.Token}, Listener: ln,
		Reality:  a.reality(priv, a.ShortID),
		Handlers: a.handlers(),
		Info:     func() ctrl.Hello { return ctrl.Hello{AgentVersion: "9.9.9", TelemtVersion: OldTelemt} },
	})
	go a.ag.Serve(ctx)
	t.Cleanup(a.ag.Close)
	for i := 0; !a.ag.Warm() && i < 300; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	return a
}

func publicOf(priv []byte) ([]byte, error) {
	k, err := ecdh.X25519().NewPrivateKey(priv)
	if err != nil {
		return nil, err
	}
	return k.PublicKey().Bytes(), nil
}

func random(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func (a *Agent) reality(priv []byte, sid [8]byte) realityserver.ServerConfig {
	names := Domains
	if a.names != nil {
		names = a.names
	}
	return realityserver.ServerConfig{Dest: a.dest, ServerNames: names, PrivateKey: priv, ShortIDs: [][8]byte{sid}, MaxTimeDiff: time.Minute}
}

// OnlyAccept narrows the SNI new connections may present, as a box moved to one cover site would.
func (a *Agent) OnlyAccept(names ...string) {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	a.names = names
	a.ag.Reconfigure(a.reality(a.priv, a.ShortID))
}

// Result is what a fake SSH install reports for this agent.
func (a *Agent) Result() *installer.Result {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	return &installer.Result{
		AgentID: a.AgentID, HubPort: a.Port, CapturePort: 9443, TelemtUID: 998,
		Creds: installer.Creds{Pub: a.Pub, ShortID: a.ShortID, Token: a.Token},
	}
}

func (a *Agent) Set(fn func(*Agent)) { a.Mu.Lock(); fn(a); a.Mu.Unlock() }

func (a *Agent) StateCount() int { a.Mu.Lock(); defer a.Mu.Unlock(); return len(a.States) }

func (a *Agent) LastState() ctrl.State {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	if len(a.States) == 0 {
		return ctrl.State{}
	}
	return a.States[len(a.States)-1]
}

// Stop takes the agent off the network.
func (a *Agent) Stop() { a.ag.Close() }

func (a *Agent) handlers() map[string]ctrl.Handler {
	hs := a.answers()
	for m, h := range hs {
		hs[m] = func(ctx context.Context, b json.RawMessage, raw io.Reader) (any, error) {
			a.Mu.Lock()
			gate := a.Gate
			a.Mu.Unlock()
			if gate != nil {
				gate(m, b)
			}
			return h(ctx, b, raw)
		}
	}
	return hs
}

func (a *Agent) answers() map[string]ctrl.Handler {
	return map[string]ctrl.Handler{
		ctrl.MStateApply: func(_ context.Context, b json.RawMessage, _ io.Reader) (any, error) {
			var s ctrl.State
			if err := json.Unmarshal(b, &s); err != nil {
				return nil, err
			}
			a.Mu.Lock()
			defer a.Mu.Unlock()
			a.States = append(a.States, s)
			a.Applied = s.Rev
			return ctrl.StateAck{Rev: s.Rev}, nil
		},
		ctrl.MStatus: func(context.Context, json.RawMessage, io.Reader) (any, error) {
			a.Mu.Lock()
			defer a.Mu.Unlock()
			var st ctrl.Status
			st.AgentVersion, st.TelemtVersion, st.StateRev = "9.9.9", OldTelemt, a.Applied
			st.UptimeSec, st.RSSBytes = 42, 12<<20
			st.Telemt.Running, st.Telemt.CrashLoop, st.Telemt.Restarts, st.Telemt.Held = a.Running, a.CrashLoop, a.Restarts, a.Held
			st.Capture.Rules = true
			// like a freshly started agent: it has not checked its cover site yet
			if n := len(a.States); n > 0 {
				st.Disguise.Domain = a.States[n-1].Disguise
			}
			return st, nil
		},
		ctrl.MStats: func(context.Context, json.RawMessage, io.Reader) (any, error) {
			a.Mu.Lock()
			defer a.Mu.Unlock()
			return ctrl.Stats{At: time.Now(), Users: append([]ctrl.UserStat(nil), a.Users...)}, nil
		},
		ctrl.MDisguiseCheck: func(_ context.Context, b json.RawMessage, _ io.Reader) (any, error) {
			var c ctrl.DisguiseCheck
			json.Unmarshal(b, &c)
			a.Mu.Lock()
			defer a.Mu.Unlock()
			a.Checked = append(a.Checked, c.Domain)
			if a.CheckErr != "" {
				return nil, &ctrl.Error{Code: "check", Msg: a.CheckErr}
			}
			if a.CheckOK {
				return ctrl.DisguiseResult{OK: true, TLS13: true, X25519: true, H2: true, CertOK: true, Addrs: []string{"192.0.2.10"}, LatencyMS: 12}, nil
			}
			return ctrl.DisguiseResult{TLS13: true, Err: "the site does not offer h2"}, nil
		},
		ctrl.MServiceRestart: func(context.Context, json.RawMessage, io.Reader) (any, error) {
			a.Mu.Lock()
			a.Restarts++
			a.Mu.Unlock()
			return map[string]bool{"ok": true}, nil
		},
		ctrl.MLogsTail: func(_ context.Context, b json.RawMessage, _ io.Reader) (any, error) {
			var req ctrl.LogsRequest
			json.Unmarshal(b, &req)
			a.Mu.Lock()
			defer a.Mu.Unlock()
			lines := a.Logs
			if req.Lines > 0 && len(lines) > req.Lines {
				lines = lines[len(lines)-req.Lines:]
			}
			return ctrl.LogsResponse{Lines: append([]string(nil), lines...)}, nil
		},
		ctrl.MUpdate: func(_ context.Context, b json.RawMessage, raw io.Reader) (any, error) {
			var req ctrl.UpdateRequest
			json.Unmarshal(b, &req)
			if raw != nil {
				io.Copy(io.Discard, raw)
			}
			a.Mu.Lock()
			a.Updates = append(a.Updates, req)
			a.Mu.Unlock()
			return map[string]bool{"ok": true}, nil
		},
		ctrl.MKeysRotate: func(_ context.Context, b json.RawMessage, _ io.Reader) (any, error) {
			var k ctrl.KeysRotate
			if err := json.Unmarshal(b, &k); err != nil {
				return nil, err
			}
			a.Mu.Lock()
			defer a.Mu.Unlock()
			if k.Finish {
				a.ag.SetTokens([][]byte{a.Token})
				return nil, nil
			}
			// like the real agent: both tokens get in until the panel finishes
			var sid [8]byte
			copy(sid[:], k.ShortIDs[0])
			pub, err := publicOf(k.PrivateKey)
			if err != nil {
				return nil, err
			}
			a.ag.SetTokens([][]byte{k.Token, a.Token})
			a.ag.Reconfigure(a.reality(k.PrivateKey, sid))
			a.Token, a.ShortID, a.Pub, a.priv = k.Token, sid, pub, k.PrivateKey
			return nil, nil
		},
		ctrl.MUninstall: func(context.Context, json.RawMessage, io.Reader) (any, error) {
			a.Mu.Lock()
			a.Uninstalls++
			a.Mu.Unlock()
			return nil, nil
		},
	}
}

// Wait polls cond for up to 14 s.
func Wait(t testing.TB, cond func() bool, what string) {
	t.Helper()
	for i := 0; i < 700; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
