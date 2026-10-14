package ctrl

import "time"

// ProtocolVersion is bumped for any incompatible change to a method's JSON.
const ProtocolVersion = 1

// Methods the hub calls on the agent.
const (
	MHello          = "hello"
	MHelloConfirm   = "hello.confirm"
	MStateApply     = "state.apply"
	MStateGet       = "state.get"
	MStatus         = "status"
	MStats          = "stats"
	MLogsTail       = "logs.tail"
	MServiceRestart = "service.restart"
	MUpdate         = "update" // header JSON + raw binary
	MUpdateCommit   = "update.commit"
	MKeysRotate     = "keys.rotate"
	MLinkPort       = "link.port"
	MDisguiseCheck  = "disguise.check"
	MUninstall      = "uninstall"
)

// Hello is the first exchange on every new link connection. It proves both
// sides hold the link token without ever sending it:
//
//	hub   -> agent  Hello{Nonce: nH}
//	agent -> hub    Hello{Nonce: nA, AgentID, MAC: HMAC(token, "agent"|nH|nA|agentID)}
//	hub   -> agent  HelloConfirm{MAC: HMAC(token, "hub"|nA|nH|agentID)}
type Hello struct {
	Version       int      `json:"v"`
	AgentID       string   `json:"agent_id,omitempty"`
	Nonce         []byte   `json:"nonce"`
	MAC           []byte   `json:"mac,omitempty"`
	AgentVersion  string   `json:"agent_version,omitempty"`
	TelemtVersion string   `json:"telemt_version,omitempty"`
	Caps          []string `json:"caps,omitempty"`
	MaxStreams    int      `json:"max_streams,omitempty"`
	Now           int64    `json:"now,omitempty"`      // agent's clock, unix ms: REALITY fails past two minutes of skew
	TokenID       []byte   `json:"token_id,omitempty"` // which token the hub holds; the agent proves with that one
}

type HelloConfirm struct {
	MAC []byte `json:"mac"`
}

// User is one telemt credential the hub wants live. Name is the telemt
// username (the hub resolves email/username before sending).
type User struct {
	Name        string    `json:"name"`
	Secret      string    `json:"secret"`
	Enabled     bool      `json:"enabled"`
	MaxIPs      int       `json:"max_ips,omitempty"`
	ExpiresAt   time.Time `json:"expires_at,omitzero"`
	RateUpBps   int64     `json:"rate_up_bps,omitempty"`
	RateDownBps int64     `json:"rate_down_bps,omitempty"`
}

// State is the complete desired state of one agent. The agent keeps no other
// source of truth: it renders telemt's config from this and replays the last
// state it accepted when it boots without the hub.
type State struct {
	Rev        string   `json:"rev"`
	Port       int      `json:"port"`             // telemt's public port (443)
	Listen     string   `json:"listen,omitempty"` // telemt bind address; empty = all
	Disguise   string   `json:"disguise"`         // REALITY dest/SNI and telemt tls_domain
	Telemt     any      `json:"telemt,omitempty"` // telemt.Settings overrides (JSON object)
	Users      []User   `json:"users"`
	DCNets     []string `json:"dc_nets"` // prefixes the capture rules redirect
	MaxStreams int      `json:"max_streams,omitempty"`
	// Stopped keeps telemt off whatever the users; the redirect stays
	Stopped bool `json:"stopped,omitempty"`
}

// HeldStopped is Status.Telemt.Held while the panel keeps telemt stopped.
const HeldStopped = "stopped from the panel"

type StateAck struct {
	Rev       string `json:"rev"`
	Restarted bool   `json:"restarted"` // telemt was restarted (core section changed)
}

type Status struct {
	AgentVersion  string `json:"agent_version"`
	AgentBuild    string `json:"agent_build,omitempty"` // SHA-256 of the running binary: dev builds share a version
	TelemtVersion string `json:"telemt_version"`
	StateRev      string `json:"state_rev"`
	UptimeSec     int64  `json:"uptime_sec"`
	RSSBytes      uint64 `json:"rss_bytes"`
	Goroutines    int    `json:"goroutines"`        // a climb here is a leak long before RSS shows it
	Warm          bool   `json:"warm"`              // REALITY post-handshake sampling finished
	BootID        string `json:"boot_id,omitempty"` // new on every agent start: tells an updated process from the old one

	Telemt struct {
		Running   bool   `json:"running"`
		Restarts  int    `json:"restarts"`
		UptimeSec int64  `json:"uptime_sec"`
		LastExit  string `json:"last_exit,omitempty"`
		CrashLoop bool   `json:"crash_loop,omitempty"`
		Held      string `json:"held,omitempty"` // why the agent keeps telemt stopped (no state applied, rules missing)
	} `json:"telemt"`
	Capture struct {
		Rules    bool   `json:"rules"` // nft table present
		Active   int    `json:"active"`
		Rejected uint64 `json:"rejected"`
		Restored int    `json:"restored,omitempty"` // times the rules were found missing and put back
	} `json:"capture"`
	Disguise struct {
		Domain    string    `json:"domain"`
		OK        bool      `json:"ok"`
		CheckedAt time.Time `json:"checked_at,omitzero"`
		Err       string    `json:"err,omitempty"`
	} `json:"disguise"`
}

type UserStat struct {
	Name       string `json:"name"`
	OctetsUp   uint64 `json:"octets_up"`   // client -> telegram, cumulative since telemt started
	OctetsDown uint64 `json:"octets_down"` // telegram -> client
	// what the user has open right now; only meaningful when Stats.Live is set
	Connections int `json:"connections,omitempty"`
	Devices     int `json:"devices,omitempty"` // distinct client addresses
}

type Stats struct {
	At    time.Time  `json:"at"`
	Users []UserStat `json:"users"`
	// Live: the agent reads per-user connections and devices; older agents leave it unset
	Live bool `json:"live,omitempty"`
}

type LogsRequest struct {
	Lines int `json:"lines"`
}

type LogsResponse struct {
	Lines []string `json:"lines"`
}

type DisguiseCheck struct {
	Domain string `json:"domain"`
}

type DisguiseResult struct {
	OK     bool     `json:"ok"`
	TLS13  bool     `json:"tls13"`
	X25519 bool     `json:"x25519"`
	H2     bool     `json:"h2"`
	CertOK bool     `json:"cert_ok"`
	Addrs  []string `json:"addrs,omitempty"`
	// Domestic is filled by the hub, which knows its local address ranges; the
	// agent leaves it unset. false means the site resolves elsewhere: warn, do not
	// block.
	Domestic  *bool  `json:"domestic,omitempty"`
	LatencyMS int    `json:"latency_ms"`
	Err       string `json:"err,omitempty"`
}

type UpdateRequest struct {
	Kind   string `json:"kind"` // "agent" | "telemt"
	Arch   string `json:"arch"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Ver    string `json:"version"`
}

type UpdateCommit struct {
	Kind string `json:"kind"`
}

// KeysRotate swaps the link credentials. The agent keeps the previous token
// beside the new one until the hub calls again with Finish set, so a hub that
// loses the reply can still get back in with the old credentials.
type KeysRotate struct {
	PrivateKey []byte   `json:"private_key,omitempty"`
	ShortIDs   [][]byte `json:"short_ids,omitempty"`
	Token      []byte   `json:"token,omitempty"`
	Finish     bool     `json:"finish,omitempty"`
}

// LinkPort moves the link: Port opens it beside the current one, Finish (sent over
// a connection to the new port) closes the old, Abort closes the new instead.
type LinkPort struct {
	Port   int  `json:"port,omitempty"`
	Finish bool `json:"finish,omitempty"`
	Abort  bool `json:"abort,omitempty"`
}
