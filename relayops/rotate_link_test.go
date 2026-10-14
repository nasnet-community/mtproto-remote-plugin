package relayops_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"github.com/nasnet-community/mtproto-remote-plugin/relayops"
	"github.com/nasnet-community/mtproto-remote-plugin/relayops/relaytest"
)

// liveLinks is a hub's Links over one real link.Hub, dialling what
// Record.LinkCreds says, the way a hub's adapter should.
type liveLinks struct {
	hub  *link.Hub
	mu   sync.Mutex
	raws []net.Conn
}

func (l *liveLinks) dial(ctx context.Context, n, a string) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, n, a)
	if err == nil {
		l.mu.Lock()
		l.raws = append(l.raws, c)
		l.mu.Unlock()
	}
	return c, err
}

// cut drops every connection, as a network blip would.
func (l *liveLinks) cut() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.raws {
		c.Close()
	}
	l.raws = nil
}

func (l *liveLinks) Ensure(context.Context, uint) error { return nil }
func (l *liveLinks) View(uint) (relayops.LinkView, bool) {
	return relayops.LinkView{Up: l.hub.Status().Up > 0}, true
}
func (l *liveLinks) IsRunning(uint) bool { return true }
func (l *liveLinks) SetCreds(_ context.Context, r *relayops.Record) error {
	c, err := r.LinkCreds()
	if err != nil {
		return err
	}
	l.hub.SetCreds(c)
	return nil
}
func (l *liveLinks) Remove(uint) {}
func (l *liveLinks) Call(ctx context.Context, _ uint, m string, req, resp any) error {
	return l.hub.Call(ctx, m, req, resp)
}
func (l *liveLinks) CallRaw(ctx context.Context, _ uint, m string, req any, body io.Reader, resp any) error {
	return l.hub.CallRaw(ctx, m, req, body, resp)
}
func (l *liveLinks) Redial(context.Context, uint) error { l.cut(); return nil }

func TestARotationWhoseReplyIsLostIsFinishedOverTheLink(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cover := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	cover.EnableHTTP2 = true
	cover.Config.ErrorLog = log.New(io.Discard, "", 0)
	cover.StartTLS()
	defer cover.Close()
	server := func(priv, sid []byte) realityserver.ServerConfig {
		var s8 [8]byte
		copy(s8[:], sid)
		return realityserver.ServerConfig{Dest: cover.Listener.Addr().String(), ServerNames: []string{"cover.example.com"},
			PrivateKey: priv, ShortIDs: [][8]byte{s8}, MaxTimeDiff: time.Minute}
	}

	priv, pub, _ := reality.GenerateKey()
	sid := []byte{4, 2}
	token := make([]byte, 32)
	rand.Read(token)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	links := &liveLinks{}

	// the agent end: switches on keys.rotate like the real one, but the first
	// reply never reaches the hub
	var (
		mu      sync.Mutex
		rotates [][]byte
	)
	var agent *link.Agent
	agent = link.NewAgent(link.AgentConfig{
		AgentID: "a1", Tokens: [][]byte{token}, Listener: ln, Reality: server(priv, sid),
		Handlers: map[string]ctrl.Handler{
			ctrl.MKeysRotate: func(_ context.Context, b json.RawMessage, _ io.Reader) (any, error) {
				var k ctrl.KeysRotate
				json.Unmarshal(b, &k)
				if k.Finish {
					return nil, nil
				}
				agent.SetTokens([][]byte{k.Token, token})
				agent.Reconfigure(server(k.PrivateKey, k.ShortIDs[0]))
				mu.Lock()
				rotates = append(rotates, k.Token)
				first := len(rotates) == 1
				mu.Unlock()
				if first {
					links.cut() // applied, and the answer is lost with the connection
				}
				return nil, nil
			},
		},
	})
	go agent.Serve(ctx)
	for i := 0; !agent.Warm() && i < 300; i++ {
		time.Sleep(50 * time.Millisecond)
	}

	store := relaytest.NewMemStore()
	row := relayops.Record{NodeID: 1, AgentID: "a1", HubPort: 8443, Disguise: "cover.example.com",
		RealityPub: hex.EncodeToString(pub), ShortID: hex.EncodeToString(sid), Token: hex.EncodeToString(token)}
	store.Save(ctx, &row)
	creds, err := row.LinkCreds()
	if err != nil {
		t.Fatal(err)
	}
	links.hub = link.NewHub(link.HubConfig{
		AgentID: "a1", Token: creds.Token, Reality: creds.Reality, Addr: ln.Addr().String(),
		Conns: []link.ConnSpec{{Name: "c", Dial: links.dial}},
	})
	go links.hub.Run(ctx)
	wctx, wcancel := context.WithTimeout(ctx, 30*time.Second)
	defer wcancel()
	if err := links.hub.WaitUp(wctx); err != nil {
		t.Fatal(err)
	}

	svc := relayops.NewService(relayops.ServiceConfig{Store: store, Links: links, LinkWait: 30 * time.Second})
	id, err := svc.StartRotate(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	var v relayops.JobView
	for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if v, _ = svc.Job(id); v.State != relayops.JobRunning {
			break
		}
	}
	if v.State != relayops.JobOK {
		t.Fatalf("job = %+v", v)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(rotates) != 2 || !bytes.Equal(rotates[0], rotates[1]) {
		t.Fatalf("the agent saw %d rotations; the repeat must be the same one", len(rotates))
	}
	after, _ := store.Get(ctx, 1)
	if after.Token != hex.EncodeToString(rotates[0]) || after.NextSeed != "" {
		t.Fatalf("stored = %+v", after)
	}
	if links.hub.Status().Up == 0 {
		t.Fatal("the hub is not back on the new credentials")
	}
}
