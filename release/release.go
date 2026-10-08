// Package release pins what the hubs install on a VPS: the agent and telemt
// binaries, by version and SHA-256. The VPS never downloads anything; a hub
// fetches these URLs, checks the hash against this file and pushes the bytes.
//
// The agent binary does not import this package, so editing the pins never
// changes the agent's own bytes. An agent release (tag agent-vX.Y.Z) is built
// first, its hashes are written here afterwards, and CI rebuilds the agent from
// any later commit with the pinned version stamp and fails if the bytes differ.
package release

import "fmt"

// Arch is a Linux CPU architecture the agent supports.
type Arch string

const (
	AMD64 Arch = "amd64"
	ARM64 Arch = "arm64"
)

// Asset is one downloadable file with the hash it must have.
type Asset struct {
	URL    string
	SHA256 string
	// Member is the file to extract when URL is a .tar.gz.
	Member string
}

const (
	// TelemtVersion matches the version the branch's heavy agent pins, so one
	// telemt build is in play across both agents.
	TelemtVersion = "3.4.25"
	// AgentVersion is the agent release these pins describe.
	AgentVersion = "0.0.0-dev"
)

// Telemt is the upstream release tarball per architecture. We link to upstream
// rather than rehost it: telemt's licence forbids presenting redistributions as
// official.
var Telemt = map[Arch]Asset{
	AMD64: {
		URL:    "https://github.com/telemt/telemt/releases/download/" + TelemtVersion + "/telemt-x86_64-linux-musl.tar.gz",
		SHA256: "33e21f17d6b54111ac9d53d85d54f191cf1e6c48cc9234ce05de5fe1bd8c5de8",
		Member: "telemt",
	},
	ARM64: {
		URL:    "https://github.com/telemt/telemt/releases/download/" + TelemtVersion + "/telemt-aarch64-linux-musl.tar.gz",
		SHA256: "7c9a2196418bc9ecc8c7e493e9cfb299410cbbc065920fe3115ba08109118eed",
		Member: "telemt",
	},
}

// Agent is filled in when an agent release is cut (tag agent-vX.Y.Z, see the
// release stage). Empty hashes make Lookup fail, so an unreleased build can never
// be installed by accident.
var Agent = map[Arch]Asset{
	AMD64: {URL: "https://github.com/nasnet-community/mtproto-remote-plugin/releases/download/agent-v" + AgentVersion + "/mtproto-agent-linux-amd64"},
	ARM64: {URL: "https://github.com/nasnet-community/mtproto-remote-plugin/releases/download/agent-v" + AgentVersion + "/mtproto-agent-linux-arm64"},
}

// ParseArch maps `uname -m` output to a supported Arch.
func ParseArch(uname string) (Arch, error) {
	switch uname {
	case "x86_64", "amd64":
		return AMD64, nil
	case "aarch64", "arm64":
		return ARM64, nil
	}
	return "", fmt.Errorf("release: %q is not supported (need x86_64 or aarch64 Linux)", uname)
}

// Lookup returns the pinned asset, refusing one with no hash.
func Lookup(m map[Arch]Asset, a Arch) (Asset, error) {
	as, ok := m[a]
	if !ok {
		return Asset{}, fmt.Errorf("release: nothing pinned for %s", a)
	}
	if len(as.SHA256) != 64 {
		return Asset{}, fmt.Errorf("release: the %s asset has no pinned hash (unreleased build)", a)
	}
	return as, nil
}
