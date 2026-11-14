package sandbox

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/proc"
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

// defaultCredentialMask returns a mask for every credential file in the checkout
// at projectPath: each regular file whose name starts `.env`, except the committed
// `.env.example` and any file git tracks (#332). The harness's own `.agent-harness/.env` is one;
// a Consumer's infra/x/.env or web/.env.local are others, and every container
// mounts the whole checkout, so hiding only the harness's file would leave the
// rest readable to a session steered by prompt injection.
//
// The empty source file lives in the OS temp dir, never in the checkout: a source
// inside the checkout would be mounted over itself. Docker creates a missing bind
// mount target, so the walk masks only files it found — it never names a path
// that is not there.
func defaultCredentialMask(projectPath string) []MaskMount {
	if projectPath == "" {
		return nil
	}
	targets := credentialFiles(projectPath)
	if len(targets) == 0 {
		return nil
	}
	empty, err := emptyMaskFile()
	if err != nil {
		// Nothing to mount means nothing is hidden, so fail loud rather than run a
		// container that can read the operator's tokens. The caller surfaces this as
		// a failed run; a silent nil here would be the exact leak the mask prevents.
		panic("agent-harness: cannot create the credential mask file: " + err.Error())
	}
	masks := make([]MaskMount, 0, len(targets))
	for _, target := range targets {
		masks = append(masks, MaskMount{Source: empty, Target: target})
	}
	return masks
}

// credentialFiles walks projectPath for the files [isCredentialFile] names. It
// stats through a symlink, as Docker's mount does, and masks only a regular file:
// mounting a file over a directory fails the container. It does not descend into
// .git or node_modules: neither holds an operator's credentials, and node_modules
// is where a large checkout's file count is. Nor does it name a file git tracks: its
// content is in .git's objects, which the sandbox reads anyway, and a mount over a
// committed `.env` would show the session an empty file that its `git add -A`
// then commits. An unreadable directory is skipped rather than failing the launch.
// config owns the harness's own credential path (config.InRepoEnvFile), which the
// pattern covers and the scaffold gitignores, so the file the harness is willing to
// READ from the repo is one it HIDES from the sandbox.
func credentialFiles(projectPath string) []string {
	var found []string
	_ = filepath.WalkDir(projectPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() && path != projectPath {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if name := d.Name(); path != projectPath && (name == ".git" || name == "node_modules") {
				return fs.SkipDir
			}
			return nil
		}
		if !isCredentialFile(d.Name()) {
			return nil
		}
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && !tracked(path) {
			found = append(found, path)
		}
		return nil
	})
	return found
}

// tracked reports whether the git checkout holding path tracks it. Outside a
// checkout, or on any git error, the file counts as untracked, so it is masked.
func tracked(path string) bool {
	return proc.Run(gitReadTimeout, "git", git.HostArgs("-C", filepath.Dir(path),
		"ls-files", "--error-unmatch", "--", ":(literal)"+filepath.Base(path))...) == nil
}

// isCredentialFile is the mask's deny-list, `.env*` (`.env`, `.env.local`,
// direnv's `.envrc`), less its allow-list, `.env.example`: the one dotenv file a
// repository commits on purpose, as documentation with no values.
func isCredentialFile(name string) bool {
	return strings.HasPrefix(name, config.EnvFileName) && name != ".env.example"
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
