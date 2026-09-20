#!/bin/sh
# Install the agent harness in one command:
#
#   curl -fsSL https://raw.githubusercontent.com/danoleary/agent-harness/main/install.sh | sh
#
# It downloads the release archive for this host, checks it against the published
# sha256, and lays it out under a prefix it can write WITHOUT sudo — a script
# piped into a shell should not be asking for root, and nothing here needs it.
#
# What lands where (prefix defaults to ~/.local):
#
#   <prefix>/bin/agent-harness            the ONLY thing added to your PATH
#   <prefix>/libexec/agent-harness/       the stage binaries + the loop launcher
#   <prefix>/share/agent-harness/         .env.example, README, CONSUMER.md, LICENSE
#
# The stage binaries are named for their role — loop, watch, review, pipeline,
# implementation, retrospective — which are far too generic to put on a shared
# PATH (`watch` alone would shadow procps' watch(1)). They go in a private
# libexec dir, and `agent-harness <command>` dispatches to them.
#
# Knobs, as flags or environment variables:
#
#   --prefix DIR    AGENT_HARNESS_PREFIX    where to install (default ~/.local)
#   --version TAG   AGENT_HARNESS_VERSION   which release (default: the latest)
#   --uninstall                             remove everything this script installs
#
# Piped into a shell, flags need `sh -s --`:
#
#   curl -fsSL …/install.sh | sh -s -- --prefix /opt/agent-harness
#
# POSIX sh on purpose: it is the one interpreter every target host has, and an
# installer that needs bash 4 is an installer that fails on stock macOS.
set -eu

REPO="${AGENT_HARNESS_REPO:-danoleary/agent-harness}"
# Overridable so a fork, a mirror, or the test suite (which serves a fixture
# release over file://) can point the download elsewhere.
BASE_URL="${AGENT_HARNESS_BASE_URL:-https://github.com/${REPO}/releases/download}"
LATEST_URL="${AGENT_HARNESS_LATEST_URL:-https://github.com/${REPO}/releases/latest}"
PREFIX="${AGENT_HARNESS_PREFIX:-${HOME}/.local}"
VERSION="${AGENT_HARNESS_VERSION:-}"
UNINSTALL=""

# The stage binaries, in the order the pipeline runs them. Kept in one place so
# the layout check, the install and the uninstall cannot drift apart.
TOOLS="implementation review retrospective pipeline loop watch"

info() { printf '%s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

usage() {
	cat <<'USAGE'
usage: install.sh [--prefix DIR] [--version TAG] [--uninstall]

  --prefix DIR    install under DIR (default: ~/.local). Needs no sudo.
  --version TAG   install a specific release, e.g. v0.3.0 (default: the latest)
  --uninstall     remove an installation made by this script
  --help          this message
USAGE
}

parse_args() {
	while [ "$#" -gt 0 ]; do
		case "$1" in
		--prefix)
			[ "$#" -ge 2 ] || die "--prefix needs a directory"
			PREFIX="$2"
			shift 2
			;;
		--prefix=*)
			PREFIX="${1#--prefix=}"
			shift
			;;
		--version)
			[ "$#" -ge 2 ] || die "--version needs a tag, e.g. v0.3.0"
			VERSION="$2"
			shift 2
			;;
		--version=*)
			VERSION="${1#--version=}"
			shift
			;;
		--uninstall)
			UNINSTALL=1
			shift
			;;
		--help | -h)
			usage
			exit 0
			;;
		*)
			usage >&2
			die "unknown option: $1"
			;;
		esac
	done
}

# detect_platform prints the "<os>_<arch>" half of a release asset name. The
# harness host needs docker, git and gh, so the supported set is exactly the set
# the release workflow builds; anything else must fail HERE, with the reason,
# rather than downloading a 404 page and unpacking garbage.
detect_platform() {
	os="$(uname -s)"
	arch="$(uname -m)"
	case "$os" in
	Darwin) os="darwin" ;;
	Linux) os="linux" ;;
	*) die "unsupported OS: $os (the harness host needs docker, git and gh — macOS or Linux, or WSL on Windows)" ;;
	esac
	case "$arch" in
	x86_64 | amd64) arch="amd64" ;;
	arm64 | aarch64) arch="arm64" ;;
	*) die "unsupported architecture: $arch (released builds are amd64 and arm64)" ;;
	esac
	printf '%s_%s\n' "$os" "$arch"
}

# fetch downloads url to dest. curl is preferred and wget is the fallback; a host
# with neither cannot be helped, and saying so beats a confusing empty file.
fetch() {
	if have curl; then
		curl -fsSL "$1" -o "$2"
	elif have wget; then
		wget -q -O "$2" "$1"
	else
		die "need curl or wget to download the release"
	fi
}

# latest_tag resolves the newest release tag by following the /releases/latest
# redirect, whose target is .../releases/tag/<tag>. That is deliberately not the
# GitHub API: the API rate-limits unauthenticated callers to 60 requests an hour
# per IP, and an install one-liner that fails on a shared network is worse than
# no one-liner at all.
latest_tag() {
	resolved=""
	if have curl; then
		resolved="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$LATEST_URL" 2>/dev/null || true)"
	elif have wget; then
		resolved="$(wget -qS --spider --max-redirect=5 "$LATEST_URL" 2>&1 |
			awk 'tolower($1) == "location:" { print $2 }' | tail -n 1)"
	fi
	tag="${resolved##*/}"
	case "$tag" in
	v[0-9]*) printf '%s\n' "$tag" ;;
	*) return 1 ;;
	esac
}

# sha256_of prints the hex digest of a file, using whichever of the three usual
# tools this host has (sha256sum on Linux, shasum on macOS, openssl elsewhere).
sha256_of() {
	if have sha256sum; then
		sha256sum "$1" | awk '{print $1}'
	elif have shasum; then
		shasum -a 256 "$1" | awk '{print $1}'
	elif have openssl; then
		openssl dgst -sha256 "$1" | awk '{print $NF}'
	else
		return 1
	fi
}

# verify_checksum checks the downloaded archive against the release's
# checksums.txt. A mismatch aborts: this script's output is executed, so an
# archive that is not the one the release published must never be unpacked.
# The published lines are `<sha256>  ./<name>`, so the leading ./ (and the binary
# marker some tools emit) are stripped before comparing names.
verify_checksum() {
	archive="$1"
	checksums="$2"
	name="$3"

	expected="$(awk -v n="$name" '{ f = $2; sub(/^\*/, "", f); sub(/^\.\//, "", f); if (f == n) { print $1; exit } }' "$checksums")"
	[ -n "$expected" ] || die "checksums.txt has no entry for $name — is this a complete release?"

	actual="$(sha256_of "$archive")" ||
		die "no sha256 tool found (sha256sum, shasum or openssl) — cannot verify the download"

	[ "$actual" = "$expected" ] ||
		die "checksum mismatch for $name
  expected $expected
  actual   $actual
Refusing to install. Re-run the install; if it fails again, open an issue."
}

# install_file copies through a temporary name and renames into place, so an
# interrupted install never leaves a half-written binary that looks installed —
# and so upgrading while the loop is running replaces the file rather than
# writing into the one the daemon is executing.
install_file() {
	src="$1"
	dest="$2"
	tmp="${dest}.tmp.$$"
	cp "$src" "$tmp"
	chmod 0755 "$tmp"
	mv -f "$tmp" "$dest"
}

# install_tree lays an unpacked archive out under the prefix. It is the inverse of
# uninstall, and the two are tested against each other.
install_tree() {
	stage="$1"
	prefix="$2"

	[ -f "$stage/agent-harness" ] || die "the archive has no agent-harness binary — is this a harness release?"

	mkdir -p "$prefix/bin" "$prefix/libexec/agent-harness" "$prefix/share/agent-harness"

	install_file "$stage/agent-harness" "$prefix/bin/agent-harness"
	for tool in $TOOLS; do
		[ -f "$stage/libexec/agent-harness/$tool" ] ||
			die "the archive is missing the $tool binary — refusing a partial install"
		install_file "$stage/libexec/agent-harness/$tool" "$prefix/libexec/agent-harness/$tool"
	done

	# The loop launcher lives beside the binaries it starts, which is where
	# `agent-harness start` looks for it.
	if [ -f "$stage/scripts/loop-start.sh" ]; then
		install_file "$stage/scripts/loop-start.sh" "$prefix/libexec/agent-harness/loop-start.sh"
	fi

	# The docs and the credential template travel with the install: an operator
	# who ran the one-liner has no checkout to copy .env.example out of, and that
	# copy is the documented first step.
	for doc in README.md CONSUMER.md LICENSE .env.example; do
		if [ -f "$stage/$doc" ]; then
			cp "$stage/$doc" "$prefix/share/agent-harness/$doc"
			chmod 0644 "$prefix/share/agent-harness/$doc"
		fi
	done
}

# uninstall removes exactly what install_tree writes — named file by named file,
# never `rm -rf` on a directory derived from an environment variable. Anything
# the operator put in these directories themselves survives.
uninstall() {
	prefix="$1"
	removed=""
	for path in \
		"$prefix/bin/agent-harness" \
		"$prefix/libexec/agent-harness/loop-start.sh" \
		"$prefix/share/agent-harness/README.md" \
		"$prefix/share/agent-harness/CONSUMER.md" \
		"$prefix/share/agent-harness/LICENSE" \
		"$prefix/share/agent-harness/.env.example"; do
		if [ -e "$path" ]; then
			rm -f "$path"
			removed=1
		fi
	done
	for tool in $TOOLS; do
		if [ -e "$prefix/libexec/agent-harness/$tool" ]; then
			rm -f "$prefix/libexec/agent-harness/$tool"
			removed=1
		fi
	done
	# Only if they are now empty — rmdir refuses otherwise, which is the point.
	rmdir "$prefix/libexec/agent-harness" "$prefix/share/agent-harness" 2>/dev/null || true

	if [ -n "$removed" ]; then
		info "Removed the agent harness from $prefix."
		info "Your .env and your project's .agent-harness/ directory were left alone."
	else
		info "Nothing to remove: no agent harness install found under $prefix."
	fi
}

# on_path reports whether dir is already a PATH entry, so the install only nags
# about PATH when it actually has to.
on_path() {
	case ":${PATH}:" in
	*":$1:"*) return 0 ;;
	*) return 1 ;;
	esac
}

# shell_profile guesses the file a PATH line belongs in, from the login shell.
# It is a hint printed for the operator to run, never an edit this script makes:
# a piped-to-shell installer that silently rewrites dotfiles is not one worth
# trusting.
shell_profile() {
	case "${SHELL:-}" in
	*/zsh) printf '%s\n' "${ZDOTDIR:-$HOME}/.zshrc" ;;
	*/bash) [ -f "$HOME/.bash_profile" ] && printf '%s\n' "$HOME/.bash_profile" || printf '%s\n' "$HOME/.bashrc" ;;
	*/fish) printf '%s\n' "${XDG_CONFIG_HOME:-$HOME/.config}/fish/config.fish" ;;
	*) printf '%s\n' "$HOME/.profile" ;;
	esac
}

# check_prereqs warns about the host tools the harness needs AT RUN TIME. These
# are warnings, not errors: installing on a machine you are about to finish
# setting up is perfectly reasonable, and the harness itself fails loudly and
# specifically if one is still missing when a run starts.
check_prereqs() {
	missing=""
	have docker || missing="${missing} docker"
	have git || missing="${missing} git"
	have gh || missing="${missing} gh"
	if [ -n "$missing" ]; then
		warn "the harness needs these on the host at run time, and they are not installed:${missing}"
	fi

	# git 2.45+: the pre-push rebase replays with `git cherry-pick --empty=drop`,
	# a flag that landed in 2.45. An older git fails mid-run, at push time.
	if have git; then
		git_version="$(git --version 2>/dev/null | awk '{print $3}')"
		git_major="${git_version%%.*}"
		git_rest="${git_version#*.}"
		git_minor="${git_rest%%.*}"
		case "${git_major}${git_minor}" in
		*[!0-9]* | "") ;; # unparseable (a vendor-patched version string) — say nothing
		*)
			if [ "$git_major" -lt 2 ] || { [ "$git_major" -eq 2 ] && [ "$git_minor" -lt 45 ]; }; then
				warn "git $git_version is older than the required 2.45 (the push rebase needs 'cherry-pick --empty=drop')"
			fi
			;;
		esac
	fi
}

main() {
	parse_args "$@"

	if [ -n "$UNINSTALL" ]; then
		uninstall "$PREFIX"
		return 0
	fi

	platform="$(detect_platform)"

	if [ -z "$VERSION" ]; then
		VERSION="$(latest_tag)" ||
			die "cannot work out the latest release. Pass one explicitly:
  install.sh --version v0.3.0
or see https://github.com/${REPO}/releases"
	fi

	name="agent-harness_${VERSION}_${platform}.tar.gz"
	info "Installing agent-harness ${VERSION} (${platform}) into ${PREFIX}"

	tmp="$(mktemp -d "${TMPDIR:-/tmp}/agent-harness-install.XXXXXX")"
	# Clean up on every exit path, including a failed download.
	trap 'rm -rf "$tmp"' EXIT INT TERM

	fetch "${BASE_URL}/${VERSION}/${name}" "$tmp/$name" ||
		die "cannot download ${name}.
Does ${VERSION} have a build for ${platform}? See https://github.com/${REPO}/releases"
	fetch "${BASE_URL}/${VERSION}/checksums.txt" "$tmp/checksums.txt" ||
		die "cannot download checksums.txt for ${VERSION} — refusing to install unverified binaries"
	verify_checksum "$tmp/$name" "$tmp/checksums.txt" "$name"

	tar -xzf "$tmp/$name" -C "$tmp" || die "cannot unpack $name"
	stage="$tmp/agent-harness_${VERSION}_${platform}"
	[ -d "$stage" ] || die "unexpected archive layout: no ${stage##*/}/ directory inside $name"

	install_tree "$stage" "$PREFIX"

	installed="$PREFIX/bin/agent-harness"
	# Smoke-test the thing we just installed rather than claiming success on the
	# strength of a successful copy — a wrong-architecture binary gets this far.
	reported="$("$installed" --version 2>/dev/null || true)"
	[ -n "$reported" ] || die "installed $installed, but it will not run. Wrong architecture, or a broken download."

	info ""
	info "Installed agent-harness ${reported} -> ${installed}"
	info ""

	if ! on_path "$PREFIX/bin"; then
		info "$PREFIX/bin is not on your PATH yet. Add it:"
		info ""
		info "  echo 'export PATH=\"$PREFIX/bin:\$PATH\"' >> $(shell_profile)"
		info "  export PATH=\"$PREFIX/bin:\$PATH\""
		info ""
	fi

	check_prereqs

	cat <<NEXT
Next:

  1. agent-harness --help

  2. Give it credentials — copy the template and fill it in:
       mkdir -p ~/.config/agent-harness
       cp $PREFIX/share/agent-harness/.env.example ~/.config/agent-harness/.env

  3. Give your project an .agent-harness/ directory (config, prompts, sandbox image):
       $PREFIX/share/agent-harness/CONSUMER.md

  4. Work one ticket end to end:
       agent-harness pipeline --next
NEXT
}

# Sourced by the test suite with AGENT_HARNESS_SOURCE_ONLY set, so the functions
# above can be exercised one at a time without running an install.
if [ -z "${AGENT_HARNESS_SOURCE_ONLY:-}" ]; then
	main "$@"
fi
