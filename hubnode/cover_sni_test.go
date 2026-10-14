package hubnode

import (
	"context"
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
	"github.com/nasnet-community/mtproto-remote-plugin/link"
	"github.com/nasnet-community/mtproto-remote-plugin/reality"
	"github.com/nasnet-community/mtproto-remote-plugin/realityserver"
)

// A new cover site moves the agent's SNI the moment it applies the push; if
// that answer is lost, the hub must still get back in to hear it.
func TestALostAnswerToACoverSiteChangeDoesNotLockTheHubOut(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cover := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	cover.EnableHTTP2 = true
	cover.Config.ErrorLog = log.New(io.Discard, "", 0)
	cover.StartTLS()
	defer cover.Close()
	priv, pub, _ := reality.GenerateKey()
	sid := [8]byte{4, 2}
	server := func(name string) realityserver.ServerConfig {
		return realityserver.ServerConfig{Dest: cover.Listener.Addr().String(), ServerNames: []string{name},
			PrivateKey: priv, ShortIDs: [][8]byte{sid}, MaxTimeDiff: time.Minute}
	}
	token := make([]byte, 32)
	rand.Read(token)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")

	var mu sync.Mutex
	var raws []net.Conn
	var applied string
	cut := func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range raws {
			c.Close()
		}
		raws = nil
	}
	dial := func(ctx context.Context, n, a string) (net.Conn, error) {
		var d net.Dialer
		c, err := d.DialContext(ctx, n, a)
		if err == nil {
			mu.Lock()
			raws = append(raws, c)
			mu.Unlock()
		}
		return c, err
	}
	var agent *link.Agent
	cutOnce := sync.OnceFunc(cut)
	agent = link.NewAgent(link.AgentConfig{
		AgentID: "a1", Tokens: [][]byte{token}, Listener: ln, Reality: server("old.example.com"),
		Handlers: map[string]ctrl.Handler{
			ctrl.MStateApply: func(_ context.Context, b json.RawMessage, _ io.Reader) (any, error) {
				var s ctrl.State
				json.Unmarshal(b, &s)
				agent.Reconfigure(server(s.Disguise))
				mu.Lock()
				applied = s.Rev
				mu.Unlock()
				if s.Disguise == "new.example.com" {
					cutOnce() // switched, and the answer is lost with the connection
				}
				return ctrl.StateAck{Rev: s.Rev}, nil
			},
			ctrl.MStatus: func(context.Context, json.RawMessage, io.Reader) (any, error) {
				mu.Lock()
				defer mu.Unlock()
				return ctrl.Status{StateRev: applied}, nil
			},
		},
	})
	go agent.Serve(ctx)
	for i := 0; !agent.Warm() && i < 300; i++ {
		time.Sleep(50 * time.Millisecond)
	}

	n := New(Config{
		Link: link.HubConfig{
			AgentID: "a1", Token: token, Addr: ln.Addr().String(),
			Reality: reality.ClientConfig{ServerName: "old.example.com", PublicKey: pub, ShortID: sid},
			Conns:   []link.ConnSpec{{Name: "c", Dial: dial}},
		},
		Tick: 50 * time.Millisecond, StatusEvery: 100 * time.Millisecond, PushTimeout: 3 * time.Second,
	})
	n.SetDesired(ctrl.State{Rev: "r1", Disguise: "old.example.com"})
	go n.Run(ctx)
	waitApplied := func(rev string) {
		t.Helper()
		for deadline := time.Now().Add(45 * time.Second); n.Snapshot().AppliedRev != rev; time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%s never confirmed: %+v", rev, n.Snapshot().Link)
			}
		}
	}
	waitApplied("r1")
	n.SetDesired(ctrl.State{Rev: "r2", Disguise: "new.example.com"})
	waitApplied("r2")
}
