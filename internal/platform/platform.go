// Package platform holds everything that differs between a Linux and a macOS fleet
// box: how the OS is prepared and how the two long-running jobs (the Multica daemon
// and the periodic `fleet sync`) are installed, started and stopped. The commands
// themselves still run through internal/shell, so --dry-run covers both platforms.
package platform

import (
	"fmt"
	"os"
	"runtime"

	"github.com/noelzappy/fleet/internal/config"
)

// Step is one idempotent unit of setup: Check exits 0 when it is already done,
// Apply makes it so, and Check runs again afterwards.
type Step struct {
	Name  string
	Check string
	Apply string
}

// Job is one managed process. Exec may start with "~/" for the user's home; each
// platform substitutes its own spelling (%h for systemd, $HOME for launchd).
type Job struct {
	Name        string // unit / launchd label
	Description string
	Exec        string
	WorkingDir  string
	Interval    string // set on the periodic job: a Go duration such as "2m"
}

// Spec is the pair of jobs fleet manages.
type Spec struct {
	Daemon Job
	Sync   Job
}

// File is a rendered service definition to write.
type File struct {
	Path string
	Data []byte
	Mode os.FileMode
}

type Platform interface {
	Name() string
	// BootstrapSteps prepares the OS: packages, docker, node, gh, tailscale, hardening, dirs.
	BootstrapSteps(m config.Machine, projectRoot string) []Step
	// Notes are things bootstrap can't do for you; printed after it runs.
	Notes(m config.Machine) []string
	// ServiceFiles renders the daemon and sync jobs for the service manager.
	ServiceFiles(spec Spec) ([]File, error)
	// JobFiles renders one always-on job (the Telegram bot) as a service definition.
	JobFiles(job Job) ([]File, error)
	// ReloadCmd makes the service manager see freshly written files. Idempotent.
	ReloadCmd(spec Spec) string
	// Jobs are the names StartCmd/StopCmd act on together (the daemon and the timer).
	Jobs(spec Spec) []string
	StartCmd(jobs ...string) string
	StopCmd(jobs ...string) string
	// IsActiveCmd prints a one-word state (active, inactive, ...) and never fails.
	IsActiveCmd(job string) string
	// ActiveCheck exits 0 iff the job is running (or, for the timer, loaded).
	ActiveCheck(job string) string
	// ProfileFiles are the login-shell profiles fleet adds PATH blocks to; every fleet
	// command, service and agent runs through `bash -lc`, which reads the first.
	ProfileFiles() []string
}

// Current returns the platform for runtime.GOOS, or an error on unsupported OSes.
func Current() (Platform, error) {
	return For(runtime.GOOS)
}

func For(goos string) (Platform, error) {
	switch goos {
	case "linux":
		return Linux{}, nil
	case "darwin":
		return Darwin{}, nil
	}
	return nil, fmt.Errorf("fleet boxes run Linux or macOS, not %s", goos)
}

const (
	// FnmDir is where node versions live on both platforms, so PATH lines are identical.
	FnmDir = "$HOME/.local/share/fnm"
	// ProfileMarker tags the PATH block fleet adds to the login-shell profile.
	ProfileMarker = "# fleet: node via fnm"
)

// nodeSteps are shared by both platforms: node via fnm (in FnmDir), then pnpm.
// fnm is how to invoke the fnm binary before the profile puts it on PATH.
func nodeSteps(m config.Machine, fnm string) []Step {
	return []Step{
		{
			Name:  "node " + m.NodeVersion,
			Check: "node --version 2>/dev/null | grep -q " + quote("^v"+m.NodeVersion+`\.`),
			Apply: "FNM_DIR=" + FnmDir + " " + fnm + " install " + quote(m.NodeVersion) + " && FNM_DIR=" + FnmDir + " " + fnm + " default " + quote(m.NodeVersion),
		},
		{
			Name:  "pnpm " + m.Pnpm,
			Check: "pnpm --version 2>/dev/null | grep -q " + quote("^"+m.Pnpm+`\.`),
			Apply: "npm i -g " + quote("pnpm@"+m.Pnpm),
		},
	}
}

func sshSteps(reload string) []Step {
	return []Step{
		{
			// Guard for the next step: never disable passwords on a box you can't key into.
			Name:  "SSH key installed for this user",
			Check: "test -s ~/.ssh/authorized_keys",
			Apply: `echo "no ~/.ssh/authorized_keys for $USER — install your key (ssh-copy-id) before fleet disables password login" >&2; exit 1`,
		},
		{
			// A drop-in, not an sshd_config edit: sshd keeps the first value it reads and
			// includes sshd_config.d/*.conf first. 00- sorts before cloud-init's 50- and
			// macOS's 100-macos.conf.
			Name:  "SSH password login disabled",
			Check: `sudo sshd -T 2>/dev/null | grep -qx 'passwordauthentication no'`,
			Apply: `printf 'PasswordAuthentication no\nKbdInteractiveAuthentication no\n' | sudo tee /etc/ssh/sshd_config.d/00-fleet.conf >/dev/null && sudo sshd -t && ` + reload,
		},
	}
}

func turboStep(m config.Machine) []Step {
	if m.TurboRemote == nil {
		return nil
	}
	return []Step{{
		Name:  "TURBO_TEAM in secrets file",
		Check: "grep -q '^TURBO_TEAM=' " + config.SecretsFile,
		Apply: "echo " + quote("TURBO_TEAM="+m.TurboRemote.Team) + " >> " + config.SecretsFile,
	}}
}
