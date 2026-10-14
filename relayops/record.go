// Package relayops is everything a hub does to an MTProto agent that does not
// depend on which hub it is: install, repair, update, rotate keys and remove it,
// as tracked background jobs, plus fetching the pinned binaries to install.
//
// The Linux hub and the MikroTik plugin both run this code. Each supplies its
// own storage, its own way of keeping a link open (Links) and its own notion of
// a node (NodeAdmin).
package relayops

import (
	"context"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/nasnet-community/mtproto-remote-plugin/ctrl"
	"github.com/nasnet-community/mtproto-remote-plugin/installer"
	"github.com/nasnet-community/mtproto-remote-plugin/link"
	"github.com/nasnet-community/mtproto-remote-plugin/reality"
	"github.com/nasnet-community/mtproto-remote-plugin/release"
)

// Record is what a hub remembers about one MTProto node. The REALITY private key
// is not here: it lives only on the agent.
type Record struct {
	NodeID      uint   `json:"node_id"`
	AgentID     string `json:"agent_id"`
	HubPort     int    `json:"hub_port"`
	CapturePort int    `json:"capture_port"`
	SSHPort     int    `json:"ssh_port"`
	SSHUser     string `json:"ssh_user"`
	HostKey     string `json:"host_key"`

	// Link credentials, hex encoded. Token is a secret.
	RealityPub string `json:"-"`
	ShortID    string `json:"-"`
	Token      string `json:"-"`
	// NextSeed (hex, secret): an unconfirmed rotation; the agent may hold it, so both sets are dialled.
	NextSeed string `json:"-"`
	// KeysRotatedAt is when the link credentials last changed; nil means the ones from the install.
	KeysRotatedAt *time.Time `json:"keys_rotated_at,omitempty"`

	// Disguise is the domain the agent's REALITY and telemt currently use; the
	// hub's connection must present it as its SNI.
	Disguise   string `json:"disguise"`
	MaxStreams int    `json:"max_streams"`

	Arch          string     `json:"arch"`
	AgentVersion  string     `json:"agent_version"`
	TelemtVersion string     `json:"telemt_version"`
	LastSeenAt    *time.Time `json:"last_seen_at,omitempty"`
}

var ErrNotFound = errors.New("relay: node not found")

// Store is the part of a hub's storage the service writes.
type Store interface {
	Get(ctx context.Context, nodeID uint) (*Record, error)
	Save(ctx context.Context, r *Record) error
	Delete(ctx context.Context, nodeID uint) error
}

// NodeAdmin creates and removes the hub's own record of a node (a nodes row on
// the Linux hub, an entry in the plugin's list). It never touches an agent.
type NodeAdmin interface {
	CreateNode(ctx context.Context, name, host string) (id uint, err error)
	DeleteNode(ctx context.Context, id uint) error
	// Host is the address the node's agent is reached at.
	Host(ctx context.Context, id uint) (string, error)
	// SetHost points the hub at a new address; only an address move calls it.
	SetHost(ctx context.Context, id uint, host string) error
}

// LinkView is what the service needs to know about a node's live link.
type LinkView struct {
	Up bool
	// from the agent's last status report; empty before the first
	AgentVersion, AgentBuild, TelemtVersion string
}

// Links is the hub's running connections to its agents.
type Links interface {
	Ensure(ctx context.Context, nodeID uint) error
	View(nodeID uint) (LinkView, bool)
	IsRunning(nodeID uint) bool
	SetCreds(ctx context.Context, r *Record) error
	Remove(nodeID uint)
	Call(ctx context.Context, nodeID uint, method string, req, resp any) error
	CallRaw(ctx context.Context, nodeID uint, method string, req any, body io.Reader, resp any) error
	Redial(ctx context.Context, nodeID uint) error
}

// CoverAdopter is a Links that takes a cover site a repair found on the box. Stores keep
// their own cover column over a job's possibly stale record, so this is the one way in.
type CoverAdopter interface {
	AdoptCover(ctx context.Context, nodeID uint, domain string) error
}

// SSH is the part of the installer the service drives, so tests can stand in
// for a VPS.
type SSH interface {
	// Fingerprint learns the server's host key without authenticating.
	Fingerprint(ctx context.Context, t installer.Target) (string, error)
	Arch(ctx context.Context, t installer.Target) (release.Arch, error)
	Install(ctx context.Context, t installer.Target, req installer.Request, p installer.Progress) (*installer.Result, error)
	Uninstall(ctx context.Context, t installer.Target, p installer.Progress) error
	// CheckCover tests a cover site from the VPS with nothing installed; checked
	// is false when the box can't be asked that way.
	CheckCover(ctx context.Context, t installer.Target, domain string) (res ctrl.DisguiseResult, checked bool, err error)
}

// LinkCreds is what a hub dials with: the current set, plus an unconfirmed rotation's as Next.
func (r Record) LinkCreds() (link.Creds, error) {
	c, err := hexCreds(r.Token, r.RealityPub, r.ShortID, r.Disguise)
	if err != nil {
		return link.Creds{}, fmt.Errorf("relay: node %d has unusable link credentials", r.NodeID)
	}
	if r.NextSeed != "" {
		next, err := seedCreds(r.NextSeed)
		if err != nil {
			return link.Creds{}, fmt.Errorf("relay: node %d has an unusable pending rotation", r.NodeID)
		}
		n := link.Creds{Token: next.Token, Reality: c.Reality}
		n.Reality.PublicKey, n.Reality.ShortID = next.Pub, next.ShortID
		c.Next = &n
	}
	return c, nil
}

func hexCreds(token, pub, shortID, sni string) (link.Creds, error) {
	tok, err1 := hex.DecodeString(token)
	p, err2 := hex.DecodeString(pub)
	sid, err3 := hex.DecodeString(shortID)
	if err := errors.Join(err1, err2, err3); err != nil || len(p) != 32 || len(sid) == 0 || len(sid) > 8 || len(tok) < 16 {
		return link.Creds{}, errors.New("relay: unusable link credentials")
	}
	var s8 [8]byte
	copy(s8[:], sid)
	return link.Creds{Token: tok, Reality: reality.ClientConfig{ServerName: sni, PublicKey: p, ShortID: s8}}, nil
}

// newSeed starts a rotation; one stored value lets the panel resend the very same one later.
func newSeed() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func seedCreds(seed string) (installer.Creds, error) {
	raw, err := hex.DecodeString(seed)
	if err != nil || len(raw) != 32 {
		return installer.Creds{}, errors.New("relay: bad rotation seed")
	}
	part := func(label string, n int) []byte {
		b, err := hkdf.Key(sha256.New, raw, nil, "mtproto-relay rotation "+label, n)
		if err != nil {
			panic(err) // only for n past the hash's limit, which these are not
		}
		return b
	}
	c := installer.Creds{Priv: part("reality key", 32), Token: part("link token", 32)}
	copy(c.ShortID[:], part("short id", 8))
	if c.Pub, err = reality.PublicFromPrivate(c.Priv); err != nil {
		return installer.Creds{}, err
	}
	return c, nil
}
