package sandbox

// Role is the kind of work a sandboxed session does. The launcher exports it
// into the container as RoleEnv, and a role that must not write the worktree
// also as ReadOnlyEnv=1, so the Consumer's own Claude Code hooks can tell a
// session that hands off a commit from one that only reads (#391, ADR-0065). A
// Stop hook that checks the handoff's formatting or its uncommitted files is for
// the first kind: in a retrospective it can only push a session its contract
// forbids to write toward editing and committing.
type Role string

// The roles of the harness's sessions.
const (
	RoleImplementation Role = "implementation"
	RoleReview         Role = "review"
	RoleCIFix          Role = "ci-fix"
	RoleRebaseFix      Role = "rebase-fix"
	RoleSecurityFix    Role = "security-fix"
	RoleRetrospective  Role = "retrospective"
	RoleTriage         Role = "triage"
)

// RoleEnv and ReadOnlyEnv are the container variables that carry a session's
// Role. ReadOnlyEnv is set, to "1", only in a read-only session.
const (
	RoleEnv     = "HARNESS_SESSION_ROLE"
	ReadOnlyEnv = "HARNESS_SESSION_READ_ONLY"
)

// ReadOnly reports whether a session in this role must leave the repository as
// it found it: it reads the worktree and transcripts, writes only its dropbox,
// and commits nothing.
func (r Role) ReadOnly() bool {
	return r == RoleRetrospective || r == RoleTriage
}

// roleArgs are the `-e` pairs that tell the session its role. A session with no
// role claims none. Not secrets — passed by value.
func roleArgs(r Role) []string {
	if r == "" {
		return nil
	}
	args := []string{"-e", RoleEnv + "=" + string(r)}
	if r.ReadOnly() {
		args = append(args, "-e", ReadOnlyEnv+"=1")
	}
	return args
}
