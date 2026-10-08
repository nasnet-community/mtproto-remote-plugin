// Package installer sets the agent up on a VPS over SSH and takes it away
// again. SSH is bootstrap only: after Install the hub talks to the agent over
// the link and never needs a login again.
//
// Unlike the plugin this replaces, nothing here leaves a privileged helper or a
// sudoers rule behind, every download is checked against a pinned SHA-256 (here
// and again on the VPS), and the host key is pinned before a credential is sent.
package installer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

type Target struct {
	Host       string
	Port       int // default 22
	User       string
	Password   string // password login, also the sudo password for a non-root user
	Key        []byte // PEM private key, alternative to Password
	Passphrase []byte
	HostKey    string // pinned "SHA256:..." fingerprint; empty is only valid for Probe
	Timeout    time.Duration
	// Dial is how SSH leaves the hub (router mode pins a path); nil dials plainly.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (t Target) addr() string {
	p := t.Port
	if p == 0 {
		p = 22
	}
	return net.JoinHostPort(t.Host, strconv.Itoa(p))
}

func (t Target) timeout() time.Duration {
	if t.Timeout == 0 {
		return 20 * time.Second
	}
	return t.Timeout
}

// HostKeyError is returned when the server's key is not the pinned one.
type HostKeyError struct{ Got, Want string }

func (e *HostKeyError) Error() string {
	return fmt.Sprintf("installer: the server's host key is %s, expected %s", e.Got, e.Want)
}

var errStop = errors.New("installer: stop after host key")

// Probe learns the server's host key fingerprint without authenticating: the
// handshake is abandoned in the host-key callback, before any credential could
// be sent to a server nobody has vouched for yet.
func Probe(ctx context.Context, t Target) (string, error) {
	var fp string
	cfg := &ssh.ClientConfig{
		User: t.User,
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			fp = ssh.FingerprintSHA256(k)
			return errStop
		},
		Timeout: t.timeout(),
	}
	conn, err := t.dial(ctx)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	_, _, _, err = handshake(ctx, conn, t, cfg)
	if fp == "" {
		return "", fmt.Errorf("installer: no SSH handshake: %w", err)
	}
	return fp, nil
}

func (t Target) dial(ctx context.Context) (net.Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, t.timeout())
	defer cancel()
	dial := t.Dial
	if dial == nil {
		var d net.Dialer
		dial = d.DialContext
	}
	c, err := dial(dctx, "tcp", t.addr())
	if err != nil {
		return nil, fmt.Errorf("installer: connect: %w", err)
	}
	return c, nil
}

// handshake is bounded: a host that accepts TCP and never speaks SSH would hold the job for good.
func handshake(ctx context.Context, conn net.Conn, t Target, cfg *ssh.ClientConfig) (ssh.Conn, <-chan ssh.NewChannel, <-chan *ssh.Request, error) {
	conn.SetDeadline(time.Now().Add(t.timeout()))
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	cc, chans, reqs, err := ssh.NewClientConn(conn, t.addr(), cfg)
	if !stop() && err == nil {
		cc.Close()
		return nil, nil, nil, ctx.Err()
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, nil, ctx.Err()
		}
		return nil, nil, nil, err
	}
	conn.SetDeadline(time.Time{})
	return cc, chans, reqs, nil
}

// Conn is an authenticated SSH connection.
type Conn struct {
	c *ssh.Client
	t Target

	root     bool
	nopasswd bool // sudo needs no password
}

// Connect logs in, but only to a server whose key matches t.HostKey.
func Connect(ctx context.Context, t Target) (*Conn, error) {
	if t.HostKey == "" {
		return nil, errors.New("installer: no pinned host key: Probe the server and confirm its fingerprint first")
	}
	var auth []ssh.AuthMethod
	if len(t.Key) > 0 {
		var signer ssh.Signer
		var err error
		if len(t.Passphrase) > 0 {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(t.Key, t.Passphrase)
		} else {
			signer, err = ssh.ParsePrivateKey(t.Key)
		}
		if err != nil {
			return nil, fmt.Errorf("installer: private key: %w", err)
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if t.Password != "" {
		pw := t.Password
		auth = append(auth, ssh.Password(pw), ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
			out := make([]string, len(qs))
			for i := range out {
				out[i] = pw
			}
			return out, nil
		}))
	}
	if len(auth) == 0 {
		return nil, errors.New("installer: no password or key given")
	}
	cfg := &ssh.ClientConfig{
		User: t.User, Auth: auth, Timeout: t.timeout(),
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			if got := ssh.FingerprintSHA256(k); got != t.HostKey {
				return &HostKeyError{Got: got, Want: t.HostKey}
			}
			return nil
		},
	}
	raw, err := t.dial(ctx)
	if err != nil {
		return nil, err
	}
	cc, chans, reqs, err := handshake(ctx, raw, t, cfg)
	if err != nil {
		raw.Close()
		var hk *HostKeyError
		if errors.As(err, &hk) {
			return nil, hk
		}
		return nil, fmt.Errorf("installer: login: %w", err)
	}
	c := &Conn{c: ssh.NewClient(cc, chans, reqs), t: t}
	qctx, cancel := context.WithTimeout(ctx, t.timeout())
	defer cancel()
	out, err := c.Run(qctx, "id -u", nil)
	if err != nil {
		c.Close()
		return nil, err
	}
	c.root = strings.TrimSpace(out) == "0"
	if !c.root {
		// NOPASSWD sudo never reads stdin: a password sent anyway would land in the command's input
		_, err := c.Run(qctx, "sudo -n true", nil)
		c.nopasswd = err == nil
	}
	return c, nil
}

func (c *Conn) Close() error { return c.c.Close() }

// IsRoot reports whether the login user is root.
func (c *Conn) IsRoot() bool { return c.root }

// CommandError carries a failed command's output.
type CommandError struct {
	Cmd    string
	Stderr string
	Err    error
}

func (e *CommandError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		return fmt.Sprintf("installer: %s: %v", shortCmd(e.Cmd), e.Err)
	}
	return fmt.Sprintf("installer: %s: %v: %s", shortCmd(e.Cmd), e.Err, msg)
}
func (e *CommandError) Unwrap() error { return e.Err }

func shortCmd(c string) string {
	c = strings.Join(strings.Fields(c), " ")
	if len(c) > 80 {
		c = c[:80] + "…"
	}
	return c
}

// Run executes cmd as the login user.
func (c *Conn) Run(ctx context.Context, cmd string, stdin io.Reader) (string, error) {
	return c.run(ctx, cmd, stdin, nil)
}

func (c *Conn) run(ctx context.Context, cmd string, stdin io.Reader, prefix []byte) (string, error) {
	sess, err := c.c.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	var out, errb bytes.Buffer
	sess.Stdout, sess.Stderr = &out, &errb
	switch {
	case stdin != nil && prefix != nil:
		sess.Stdin = io.MultiReader(bytes.NewReader(prefix), stdin)
	case stdin != nil:
		sess.Stdin = stdin
	case prefix != nil:
		sess.Stdin = bytes.NewReader(prefix)
	}
	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()
	select {
	case err := <-done:
		if err != nil {
			return out.String(), &CommandError{Cmd: cmd, Stderr: errb.String(), Err: err}
		}
		return out.String(), nil
	case <-ctx.Done():
		sess.Close()
		return out.String(), ctx.Err()
	}
}

// Root executes cmd as root: directly when we are root, else through sudo with
// the login password on stdin (never on the command line).
func (c *Conn) Root(ctx context.Context, cmd string, stdin io.Reader) (string, error) {
	if c.root {
		return c.run(ctx, cmd, stdin, nil)
	}
	if c.t.Password != "" && !c.nopasswd {
		// -k: a cached sudo credential would leave the password unread on stdin
		return c.run(ctx, "sudo -k -S -p '' sh -c "+shq(cmd), stdin, []byte(c.t.Password+"\n"))
	}
	return c.run(ctx, "sudo -n sh -c "+shq(cmd), stdin, nil)
}

// shq quotes s for a POSIX shell.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
