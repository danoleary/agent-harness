package sandbox

import (
	"fmt"
	"strings"
)

// verifyPnpmStoreInImage runs `pnpm store path` inside the built image and
// asserts that pnpm *resolves* its content-addressed store under the mounted
// volume. Unlike the Dockerfile-text invariant (which only proves a plausible
// `store-dir` line is present), this measures the runtime effect: it catches a
// line that is present but wired with a prefix the image's pnpm distribution
// doesn't honour — the BEH-481/BEH-487 footgun, where corepack-run pnpm 11
// reads `pnpm_config_*` but ignores `npm_config_*` (and homebrew pnpm does the
// reverse), so only an in-image resolve can tell which one actually wins.
//
// The entrypoint is bypassed (`--entrypoint pnpm`) because it hard-requires
// HERD_PATH (the checkout mount) and re-execs under gosu; the store-dir is set
// as a uid/HOME-independent ENV, so resolving it as the image's default user is
// faithful. run is injected so the resolve/compare logic is unit-testable
// without Docker.
func verifyPnpmStoreInImage(image, mount string, run func(name string, args ...string) ([]byte, error)) error {
	// COREPACK_ENABLE_DOWNLOAD_PROMPT=0 keeps corepack's first-run pnpm fetch
	// non-interactive (it would otherwise print an "about to download" notice and
	// could wait on input); the notice still lands on the merged stream, which
	// lastNonEmptyLine strips below.
	out, err := run("docker", "run", "--rm",
		"-e", "COREPACK_ENABLE_DOWNLOAD_PROMPT=0",
		"--entrypoint", "pnpm", image, "store", "path")
	if err != nil {
		return fmt.Errorf(
			"`pnpm store path` in image %q failed — cannot verify the store-dir wiring (%s)",
			image, reasonOr(out, err),
		)
	}
	// `docker run` merges stderr into stdout; corepack's download notice precedes
	// pnpm's output, so the resolved store path is the last non-empty line.
	resolved := lastNonEmptyLine(string(out))
	if !storePathUnderMount(resolved, mount) {
		return fmt.Errorf(
			"pnpm resolves its store to %q, not under the mounted volume %q — the Dockerfile's "+
				"store-dir line is present but a silent runtime no-op for this image's pnpm "+
				"(check the config-env prefix matches what the corepack-run pnpm 11 reads: "+
				"`pnpm_config_store_dir`, not `npm_config_store_dir`) (BEH-481/BEH-487)",
			resolved, mount,
		)
	}
	return nil
}

// storePathUnderMount reports whether pnpm's resolved store path (as printed by
// `pnpm store path`) lives under the mounted volume. pnpm appends a
// store-version segment (e.g. `/v11`) to the configured store-dir, so an
// honoured wiring resolves to the mount itself or a child of it; the BEH-481
// no-op instead resolves under the container's ephemeral HOME. The `mount+"/"`
// guard keeps a mere prefix sibling (`/pnpm-store-evil`) from passing.
func storePathUnderMount(resolved, mount string) bool {
	return resolved == mount || strings.HasPrefix(resolved, mount+"/")
}

// lastNonEmptyLine returns the final non-blank, trimmed line of s. The behavioral
// check reads `pnpm store path` over docker's merged stdout+stderr, where tool
// chatter (corepack's download notice) precedes pnpm's one-line answer.
func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}
