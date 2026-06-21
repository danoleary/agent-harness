package sandbox

import (
	"path/filepath"
	"regexp"
	"testing"
)

// Every sandbox + gate container mounts a persistent Docker volume at
// PnpmStoreMountPath (BuildDockerRunArgs / BuildGateRunArgs) so a warm pnpm
// store can carry across sessions and tickets. But a mounted volume is dead
// weight unless pnpm is actually told to use it as its store-dir: with no
// wiring, pnpm falls back to its default store under the `--rm` container's
// HOME, which is ephemeral, so the implementation install, the review-session
// install, and the gate install each pay a full cold network fetch and nothing
// the volume holds is ever read (BEH-481). This turns "the volume is wired" into
// an enforced invariant so the mount can never silently regress to dead weight
// again — mirrors the Playwright version-pin invariant in this package.
//
// The wiring must use a form pnpm 11 actually honours. herd pins pnpm@11.1.3
// (run via corepack), and pnpm 11 dropped reading npm-style config: env config
// now REQUIRES the `pnpm_config_*` prefix and `.npmrc`/npm global config are no
// longer read. So a textually-plausible `ENV npm_config_store_dir=/pnpm-store`
// is silently ignored at runtime — the store-dir stays at its HOME default and
// the volume is never used. A grep that accepts any `store-dir … /pnpm-store`
// would go green on that no-op; this test instead pins the honoured forms and
// explicitly rejects the bare `npm_config_*` one.
func TestDockerfileWiresPnpmStoreToMountedVolume(t *testing.T) {
	root := repoRoot(t)
	dockerfile := mustRead(t, filepath.Join(root, "agent-harness", "Dockerfile"))

	// Accept only the forms pnpm 11 honours for pointing store-dir at the volume:
	//   ENV pnpm_config_store_dir=/pnpm-store
	//   pnpm config [--global] set store-dir /pnpm-store
	mount := regexp.QuoteMeta(PnpmStoreMountPath)
	envForm := regexp.MustCompile(`(?i)pnpm_config_store_dir[\s=]+` + mount)
	cliForm := regexp.MustCompile(`(?i)pnpm\s+config\s+(?:--global\s+)?set\s+store-dir\s+` + mount)
	if !envForm.MatchString(dockerfile) && !cliForm.MatchString(dockerfile) {
		t.Errorf(
			"Dockerfile must point pnpm's store-dir at the mounted volume %q using a "+
				"form pnpm 11 honours (e.g. `ENV pnpm_config_store_dir=%s`), otherwise the "+
				"mounted pnpm-store volume is never used and every session re-downloads "+
				"deps from a cold store (BEH-481)",
			PnpmStoreMountPath, PnpmStoreMountPath,
		)
	}

	// Guard against the pnpm-10-era `npm_config_*` prefix, which pnpm 11 ignores.
	// Anchored to an actual ENV instruction line so it flags the real footgun (a
	// mis-prefixed directive) without tripping on the word in a comment, and so
	// `pnpm_config_store_dir` — which contains "npm_config_store_dir" as a
	// substring — is not a false positive.
	if badForm := regexp.MustCompile(`(?im)^\s*ENV\s+npm_config_store_dir`); badForm.MatchString(dockerfile) {
		t.Errorf(
			"Dockerfile sets store-dir via the `npm_config_*` prefix, which pnpm 11 "+
				"(herd pins pnpm@11.1.3) no longer reads — use `pnpm_config_store_dir=%s` "+
				"instead, or the wiring is a silent no-op (BEH-481)",
			PnpmStoreMountPath,
		)
	}
}
