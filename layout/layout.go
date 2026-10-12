// Package layout is where the agent lives on a VPS. The installer writes these
// paths and the agent reads them; keeping them in one place stops the two from
// drifting apart.
package layout

const (
	AgentBin  = "/usr/local/bin/mtproto-agent"
	TelemtBin = "/usr/local/bin/telemt"
	ConfDir   = "/etc/mtproto-agent"
	StatePath = ConfDir + "/state.json"
	DataDir   = "/var/lib/mtproto-agent" // 0711: telemt passes through to its own dirs
	TelemtDir = DataDir + "/telemt"      // its config, root's and group-readable
	TelemtRun = TelemtDir + "/run"       // its working dir, its own
	TrialFile = DataDir + "/trial.json"
	// KeepUser marks a telemt account that was there before the install: no removal deletes it.
	KeepUser   = ConfDir + "/keep-user"
	UnitPath   = "/etc/systemd/system/mtproto-agent.service"
	UnitName   = "mtproto-agent"
	TelemtUser = "telemt"
	// MemLimitMiB is the agent's GOMEMLIMIT. The agent lifts a lower one itself, so
	// an update reaches boxes whose unit still says 80MiB.
	MemLimitMiB = 256
)
