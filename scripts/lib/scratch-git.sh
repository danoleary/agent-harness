#!/usr/bin/env bash
# scratch-git.sh — sourced by a script test that builds a throwaway git repo
# (#166). Sourcing it cuts git off from the operator's global and system config
# for the rest of the test, so a host that signs commits, sets a hook path or a
# default branch can't make the test fail, prompt for a key, or behave
# differently from the sandbox. It gives git a fixed identity in place of the
# operator's.
#
#   . "$(dirname "$0")/lib/scratch-git.sh"
#   scratch_git_repo "$dir"            # git init -q -b main "$dir"
#   scratch_git_repo "$dir.git" --bare
#
# check-script-test-git-config.sh fails a scripts/test-*.sh that runs `git init`
# or `git clone` without sourcing this. Must stay bash-3.2-safe.

export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.invalid
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.invalid

# scratch_git_repo DIR [git-init-args...] — a fresh repo at DIR on branch main.
scratch_git_repo() {
    local dir="$1"
    shift
    git init -q -b main "$@" "$dir"
}
