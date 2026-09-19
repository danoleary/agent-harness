package sandbox

import (
	"os"
	"path/filepath"

	"github.com/danoleary/agent-harness/internal/config"
)

// MaskMount is a read-only bind mount that hides Target — a path inside the
// mounted checkout — by mounting Source over it. Docker applies the deeper mount
// after the checkout mount, so the container sees Source's content at Target.
type MaskMount struct {
	Source string
	Target string
}

// credentialMaskLookup resolves the masks for a checkout. A package var because
// the resolution stats the filesystem while the arg builders are otherwise pure;
// tests inject a fixed answer rather than laying down real files.
//
// It is deliberately NOT a field on Config/GateConfig. Every container that mounts
// the checkout must carry the mask, and a per-call-site field is a security
// invariant that a future call site can silently forget. Deriving it from
// ProjectPath inside the builders makes masking the default rather than an opt-in.
var credentialMaskLookup = defaultCredentialMask

// defaultCredentialMask returns the mask for <projectPath>/.agent-harness/.env
// when that file exists, and nothing otherwise.
//
// The empty source file lives in the OS temp dir, never in the checkout: a source
// inside the checkout would be mounted over itself. Docker creates a missing bind
// mount target, so masking a path that does not exist would litter a stray empty
// .env into a Consumer checkout that never had one — hence the stat.
func defaultCredentialMask(projectPath string) []MaskMount {
	if projectPath == "" {
		return nil
	}
	// config owns the path so the host-side lookup and this container-side mask
	// cannot drift: the file the harness is willing to READ from the repo must be
	// exactly the file it HIDES from the sandbox.
	target := config.InRepoEnvFile(projectPath)
	info, err := os.Stat(target)
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	empty, err := emptyMaskFile()
	if err != nil {
		// Nothing to mount means nothing is hidden, so fail loud rather than run a
		// container that can read the operator's tokens. The caller surfaces this as
		// a failed run; a silent nil here would be the exact leak the mask prevents.
		panic("agent-harness: cannot create the credential mask file: " + err.Error())
	}
	return []MaskMount{{Source: empty, Target: target}}
}

// maskFileName is the fixed name of the empty file mounted over a credential file.
// Fixed rather than random so repeated runs reuse one file instead of filling the
// temp dir, and so an operator inspecting a `docker run` line sees a self-describing
// path.
const maskFileName = "agent-harness-masked-credentials"

// emptyMaskFile returns the path to an empty regular file, creating it if needed.
//
// The name is fixed, so the file outlives the process and every run after the
// first finds it already there. An already-empty regular file is reused as-is;
// anything else is truncated, because content at this path would be readable in
// the container at the masked path. The mode is owner-writable so a later run can
// still truncate it — a read-only file here would make every subsequent run fail
// to build a mask, and failing to mask is the one outcome that leaks the tokens.
func emptyMaskFile() (string, error) {
	path := filepath.Join(os.TempDir(), maskFileName)

	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Size() == 0 {
		return path, nil
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		// A leftover file with a stricter mode cannot be reopened for writing, and
		// its mode came from an older harness that created it 0400. Relax it and
		// retry once rather than leaving the operator permanently unmaskable.
		if chmodErr := os.Chmod(path, 0o600); chmodErr != nil {
			return "", err
		}
		f, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return "", err
		}
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return path, nil
}

// maskArgs renders the masks for projectPath as docker `-v` arguments. Both
// container builders call it immediately after the checkout mount.
func maskArgs(projectPath string) []string {
	var args []string
	for _, m := range credentialMaskLookup(projectPath) {
		args = append(args, "-v", m.Source+":"+m.Target+":ro")
	}
	return args
}
