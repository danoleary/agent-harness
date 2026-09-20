package hostio

// baseBranch / baseRef name the integration branch every stage rebases onto,
// pushes against, and opens PRs into. They were spelled as separate "origin/main"
// / "main" string literals across the stage bodies; naming them here keeps the
// harness's one assumption about the base in one place.
const (
	baseBranch = "main"
	baseRef    = "origin/" + baseBranch
)
