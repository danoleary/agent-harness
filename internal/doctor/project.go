package doctor

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/danoleary/agent-harness/internal/config"
)

// consumerDocs is where every project-config fix points.
const consumerDocs = "https://github.com/danoleary/agent-harness/blob/main/docs/CONSUMER.md"

// Where an operator makes the tracker credentials. The same links end the setup
// errors trackers.New raises.
const (
	linearKeyURL = "https://linear.app/settings/account/security"
	jiraTokenURL = "https://id.atlassian.com/manage-profile/security/api-tokens"
)

// project is the Consumer's half as doctor saw it: the loaded config when it
// loaded, and why not when it did not, so the checks that need it can skip with
// the reason instead of failing on their own.
type project struct {
	path          string
	cfg           config.ProjectConfig
	loaded        bool
	missing       string // why cfg is unusable, for the dependents' Skip
	configResult  Result
	promptsResult Result
}

func loadProject(p Probes) project {
	pr := project{path: p.Getenv("PROJECT_PATH")}
	if config.ProjectPathProblem(pr.path) != "" {
		pr.missing = "needs a valid PROJECT_PATH"
		pr.configResult = Result{Name: "config.toml", Status: Skip, Detail: pr.missing}
		pr.promptsResult = Result{Name: "prompt bodies", Status: Skip, Detail: pr.missing}
		return pr
	}

	cfg, err := config.LoadProject(pr.path)
	switch {
	case err != nil:
		pr.missing = "needs a loadable config.toml"
		pr.configResult = Result{Name: "config.toml", Status: Fail, Detail: err.Error(), Fix: "see " + consumerDocs}
	case len(cfg.Warnings) > 0:
		pr.configResult = Result{Name: "config.toml", Status: Warn, Detail: strings.Join(cfg.Warnings, "\n"),
			Fix: "remove or correct the keys it does not know"}
	default:
		pr.configResult = Result{Name: "config.toml", Status: OK,
			Detail: fmt.Sprintf("%s (tracker %s)", filepath.Join(config.ProjectDir(pr.path), "config.toml"), cfg.Tracker.Kind)}
	}
	pr.cfg, pr.loaded = cfg, err == nil

	if _, err := config.LoadPrompts(pr.path); err != nil {
		pr.promptsResult = Result{Name: "prompt bodies", Status: Fail, Detail: err.Error(),
			Fix: "write each missing body; each names the skill its stage invokes"}
	} else {
		pr.promptsResult = Result{Name: "prompt bodies", Status: OK, Detail: "implement.md, review.md, retro.md"}
	}
	return pr
}

// checkTracker checks the tracker credential is accepted and that what the
// config names — the repo, team or project, and the labels — exists there.
func checkTracker(p Probes, pr project) Result {
	const name = "tracker"
	if !pr.loaded {
		return Result{Name: name, Status: Skip, Detail: pr.missing}
	}
	tc := pr.cfg.Tracker
	switch tc.Kind {
	case "github":
		return checkGitHubTracker(p, tc)
	case "linear":
		return checkLinearTracker(p, tc)
	case "jira":
		return checkJiraTracker(p, tc)
	}
	return Result{Name: name, Status: Fail, Detail: fmt.Sprintf("unsupported tracker kind %q", tc.Kind),
		Fix: "set tracker.kind to linear, github or jira"}
}

// labelNames are the configured labels that must exist, without blanks or repeats.
func labelNames(names ...string) []string {
	var out []string
	for _, n := range names {
		if n != "" && !contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

func checkGitHubTracker(p Probes, tc config.TrackerConfig) Result {
	const name = "tracker"
	owner, repo, err := config.SplitOwnerRepo(tc.Repo)
	if err != nil {
		return Result{Name: name, Status: Fail, Detail: "tracker." + err.Error(), Fix: "set tracker.repo to the GitHub owner/name"}
	}
	if p.Getenv("GH_TOKEN") == "" {
		return Result{Name: name, Status: Skip, Detail: "needs GH_TOKEN"}
	}
	if p.Offline {
		return Result{Name: name, Status: Skip, Detail: "GitHub Issues on " + tc.Repo + "; not verified (--offline)"}
	}
	headers := map[string]string{
		"authorization": "Bearer " + p.Getenv("GH_TOKEN"),
		"accept":        "application/vnd.github+json",
	}
	base := fmt.Sprintf("%s/repos/%s/%s", p.GitHubAPI, owner, repo)
	res, err := get(p, base, headers)
	if err != nil {
		return unreachable(name, "GitHub", err)
	}
	if res.StatusCode >= 300 {
		return Result{Name: name, Status: Fail, Detail: fmt.Sprintf("repo %s not found, or GH_TOKEN cannot see it (HTTP %d)", tc.Repo, res.StatusCode),
			Fix: "check tracker.repo, and that GH_TOKEN's owner has access to it"}
	}

	var missing []string
	for _, l := range labelNames(tc.ReadyLabel, tc.InProgressLabel, tc.BlockedLabel, tc.FindingsLabelID) {
		res, err := get(p, base+"/labels/"+url.PathEscape(l), headers)
		if err != nil {
			return unreachable(name, "GitHub", err)
		}
		switch {
		case res.StatusCode == http.StatusNotFound:
			missing = append(missing, l)
		case res.StatusCode >= 300:
			// Not a missing label, but not a present one either (a rate limit, an
			// outage): an unanswered check must not read as a pass.
			return Result{Name: name, Status: Warn, Detail: fmt.Sprintf("could not verify label %s: GitHub answered HTTP %d", l, res.StatusCode)}
		}
	}
	if len(missing) > 0 {
		var creates []string
		for _, l := range missing {
			creates = append(creates, fmt.Sprintf("gh label create %q --repo %s", l, tc.Repo))
		}
		return Result{Name: name, Status: Fail,
			Detail: fmt.Sprintf("%s has no label %s", tc.Repo, strings.Join(missing, ", ")),
			Fix:    "create it: " + strings.Join(creates, "; ")}
	}
	return Result{Name: name, Status: OK, Detail: "GitHub Issues on " + tc.Repo + ", labels present"}
}

// linearQuery reads, in one round-trip, the caller (so a bad key fails), the
// configured team, and the configured labels by name.
const linearQuery = `query Doctor($team: String!, $labels: [String!]) {
  viewer { id }
  teams(filter: { key: { eq: $team } }) { nodes { id } }
  issueLabels(filter: { name: { in: $labels } }) { nodes { name } }
}`

func checkLinearTracker(p Probes, tc config.TrackerConfig) Result {
	const name = "tracker"
	key := p.Getenv("LINEAR_API_KEY")
	if key == "" {
		return Result{Name: name, Status: Fail, Detail: "LINEAR_API_KEY is not set; tracker.kind = \"linear\" reads and claims tickets with it",
			Fix: "create a personal API key at " + linearKeyURL + " and set LINEAR_API_KEY to it"}
	}
	if p.Offline {
		return Result{Name: name, Status: Skip, Detail: "Linear team " + tc.TeamKey + "; not verified (--offline)"}
	}
	labels := labelNames(tc.ReadyLabel, tc.BlockedLabel, tc.FindingsLabel)
	body, _ := json.Marshal(map[string]any{"query": linearQuery, "variables": map[string]any{"team": tc.TeamKey, "labels": labels}})
	req, err := http.NewRequest(http.MethodPost, p.LinearAPI, bytes.NewReader(body))
	if err != nil {
		return unreachable(name, "Linear", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", key)
	res, err := p.HTTP.Do(req)
	if err != nil {
		return unreachable(name, "Linear", err)
	}
	defer res.Body.Close()

	var parsed struct {
		Data struct {
			Teams struct {
				Nodes []struct{} `json:"nodes"`
			} `json:"teams"`
			IssueLabels struct {
				Nodes []struct {
					Name string `json:"name"`
				} `json:"nodes"`
			} `json:"issueLabels"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	decodeErr := json.NewDecoder(res.Body).Decode(&parsed)
	if res.StatusCode >= 300 || len(parsed.Errors) > 0 || decodeErr != nil {
		reason := fmt.Sprintf("HTTP %d", res.StatusCode)
		if len(parsed.Errors) > 0 {
			reason = parsed.Errors[0].Message
		}
		return Result{Name: name, Status: Fail, Detail: "LINEAR_API_KEY was rejected (" + reason + ")",
			Fix: "create a personal API key at " + linearKeyURL + " and set LINEAR_API_KEY to it"}
	}
	if len(parsed.Data.Teams.Nodes) == 0 {
		return Result{Name: name, Status: Fail, Detail: "no Linear team with key " + tc.TeamKey + " is visible to LINEAR_API_KEY",
			Fix: "set tracker.team_key to the team's key (the prefix of its issue ids)"}
	}
	var found []string
	for _, n := range parsed.Data.IssueLabels.Nodes {
		found = append(found, n.Name)
	}
	var missing []string
	for _, l := range labels {
		if !contains(found, l) {
			missing = append(missing, l)
		}
	}
	if len(missing) > 0 {
		return Result{Name: name, Status: Fail, Detail: "Linear has no label " + strings.Join(missing, ", "),
			Fix: "create it in Linear (Settings → Labels), or correct the label name in config.toml"}
	}
	return Result{Name: name, Status: OK, Detail: "Linear team " + tc.TeamKey + ", labels present"}
}

func checkJiraTracker(p Probes, tc config.TrackerConfig) Result {
	const name = "tracker"
	base := strings.TrimRight(p.Getenv("JIRA_BASE_URL"), "/")
	email, token := p.Getenv("JIRA_EMAIL"), p.Getenv("JIRA_API_TOKEN")
	var unset []string
	for _, v := range [][2]string{{"JIRA_BASE_URL", base}, {"JIRA_EMAIL", email}, {"JIRA_API_TOKEN", token}} {
		if v[1] == "" {
			unset = append(unset, v[0])
		}
	}
	fix := "create an API token at " + jiraTokenURL + " and set JIRA_BASE_URL, JIRA_EMAIL and JIRA_API_TOKEN"
	if len(unset) > 0 {
		return Result{Name: name, Status: Fail, Detail: strings.Join(unset, ", ") + " not set; tracker.kind = \"jira\" signs in with them", Fix: fix}
	}
	if p.Offline {
		return Result{Name: name, Status: Skip, Detail: "Jira project " + tc.ProjectKey + "; not verified (--offline)"}
	}
	headers := map[string]string{
		"authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte(email+":"+token)),
		"accept":        "application/json",
	}
	res, err := get(p, base+"/rest/api/3/myself", headers)
	if err != nil {
		return unreachable(name, "Jira", err)
	}
	if res.StatusCode >= 300 {
		return Result{Name: name, Status: Fail, Detail: fmt.Sprintf("Jira rejected JIRA_EMAIL / JIRA_API_TOKEN (HTTP %d)", res.StatusCode), Fix: fix}
	}
	res, err = get(p, base+"/rest/api/3/project/"+url.PathEscape(tc.ProjectKey), headers)
	if err != nil {
		return unreachable(name, "Jira", err)
	}
	if res.StatusCode >= 300 {
		return Result{Name: name, Status: Fail, Detail: fmt.Sprintf("no Jira project %s visible to %s (HTTP %d)", tc.ProjectKey, email, res.StatusCode),
			Fix: "set tracker.project_key to the project's key, and check the account can browse it"}
	}
	return Result{Name: name, Status: OK, Detail: "Jira project " + tc.ProjectKey + " as " + email}
}

// checkImage checks the sandbox image can be had: present locally, buildable from
// the Consumer's Dockerfile, or pullable from its registry. It never builds or
// pulls — that is minutes of work a check should not start — it only asks.
func checkImage(p Probes, pr project, daemonUp bool) Result {
	const name = "sandbox image"
	switch {
	case !pr.loaded:
		return Result{Name: name, Status: Skip, Detail: pr.missing}
	case !daemonUp:
		return Result{Name: name, Status: Skip, Detail: "needs the docker daemon"}
	}
	image := p.Getenv("HARNESS_IMAGE")
	if image == "" {
		image = pr.cfg.Image
	}
	if _, err := p.Run("docker", "image", "inspect", image); err == nil {
		return Result{Name: name, Status: OK, Detail: image + " present locally"}
	}
	if pr.cfg.Dockerfile != "" {
		dockerfile := filepath.Join(pr.path, pr.cfg.Dockerfile)
		if !fileExists(dockerfile) {
			return Result{Name: name, Status: Fail, Detail: fmt.Sprintf("%s is not built and its Dockerfile %s does not exist", image, dockerfile),
				Fix: "commit the Dockerfile, or correct `dockerfile` in config.toml"}
		}
		return Result{Name: name, Status: OK, Detail: fmt.Sprintf("%s not built yet; the first run builds it from %s", image, dockerfile)}
	}
	if p.Offline {
		return Result{Name: name, Status: Skip, Detail: image + " not present locally; pullability not checked (--offline)"}
	}
	if out, err := p.Run("docker", "manifest", "inspect", image); err != nil {
		return Result{Name: name, Status: Fail, Detail: fmt.Sprintf("%s is not present locally and cannot be pulled (%s)", image, firstLine(out, err)),
			Fix: fmt.Sprintf("check the ref, `docker login` for a private registry, then `docker pull %s`", image)}
	}
	return Result{Name: name, Status: OK, Detail: image + " not present locally; pullable, the first run pulls it"}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// fileExists reports whether path is an existing regular file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
