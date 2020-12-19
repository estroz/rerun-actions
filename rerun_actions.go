package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/google/go-github/v33/github"
	actions "github.com/sethvargo/go-githubactions"
	"golang.org/x/oauth2"
)

const testAll = "__all"

type handler struct {
	*github.Client
	*actions.Action

	allowUserRegexps []*regexp.Regexp
	denyUserRegexps  []*regexp.Regexp
}

func (h *handler) handle(ctx context.Context, repoOwner, repoName string, commentID int64) error {
	comment, _, err := h.Issues.GetComment(ctx, repoOwner, repoName, commentID)
	if err != nil {
		h.Errorf("Failed to get comment: %v", err)
		return nil
	}
	h.Debugf("Comment %d found", comment.GetID())

	issue, _, err := h.getIssueForComment(ctx, comment)
	if err != nil {
		h.Errorf("Failed to get issue: %v", err)
		return nil
	}
	h.Debugf("Issue %d found", issue.GetID())

	if !isIssueRerunable(issue) {
		h.Debugf("Issue is not a PR or is locked")
		return nil
	}

	if !hasOkToTestLabel(issue) {
		h.Debugf("Issue lacks the \"ok-to-test\" label: %s", issue.Labels)
		return nil
	}

	// Check allow and deny lists if present.
	user := comment.GetUser().GetLogin()
	if !h.canUserComment(user) {
		h.Debugf("Issue comment was created by ignored user %s", user)
		return nil
	}

	prNum := issue.GetNumber()
	pr, _, err := h.PullRequests.Get(ctx, repoOwner, repoName, prNum)
	if err != nil {
		h.Errorf("Failed to get PR: %v", err)
		return nil
	}

	// Can't rerun actions on merged PRs.
	if pr.GetMerged() {
		h.Debugf("PR has been merged, cannot rerun workflows")
		return nil
	}

	testsToRerun := parseCommentsToWorkflowNames(comment.GetBody())
	h.Debugf("Raw comment:\n%s\n\nParsed:\n%s\n", comment.GetBody(), testsToRerun)

	opts := &github.ListOptions{}
	allWorkflows, _, err := h.Actions.ListWorkflows(ctx, repoOwner, repoName, opts)
	if err != nil {
		h.Errorf("Failed to list workflows: %v", err)
		return nil
	}

	var workflows []*github.Workflow
	if _, rerunAll := testsToRerun[testAll]; rerunAll {
		h.Debugf("Attempting to rerun all workflows")
		workflows = allWorkflows.Workflows
	} else {
		for _, workflow := range allWorkflows.Workflows {
			if _, hasWorkflow := testsToRerun[workflow.GetName()]; hasWorkflow {
				h.Debugf("Workflow %s found", workflow.GetName())
				workflows = append(workflows, workflow)
			} else {
				h.Debugf("Workflow %s not found", workflow.GetName())
			}
		}
	}

	var runsToRerun []*github.WorkflowRun
	for _, workflow := range workflows {
		h.Debugf("Workflow name: %s (%s)", workflow.GetName(), workflow.GetPath())
		// Always skip this workflow to prevent recursion issues.
		if wfName := os.Getenv("GITHUB_WORKFLOW"); wfName == workflow.GetName() || wfName == workflow.GetPath() {
			h.Debugf("Skipping the rerun workflow")
			continue
		}
		// Do not attempt to rerun inactive workflows.
		if workflow.GetState() != "active" {
			h.Debugf("Inactive workflow: %s", workflow.GetName())
			continue
		}
		opts := &github.ListWorkflowRunsOptions{
			// Filter by whoever created the PR.
			Actor: issue.GetUser().GetLogin(),
			// Filter on pull request runs.
			Event: "pull_request",
		}
		// TODO: paginate
		workflowRuns, _, err := h.Actions.ListWorkflowRunsByID(ctx, repoOwner, repoName, workflow.GetID(), opts)
		if err != nil {
			h.Errorf("Failed to list workflow runs: %v", err)
			return nil
		}
		for _, run := range workflowRuns.WorkflowRuns {
			// Stop searching runs once an older run is found (assuming they are linearized by time).
			if run.GetCreatedAt().Before(pr.GetCreatedAt()) {
				h.Debugf("Older workflow run than PR %d found", prNum)
				break
			}
			if run.GetHeadSHA() == pr.GetHead().GetSHA() {
				h.Debugf("Found run matching PR %d SHA %s", prNum, pr.GetHead().GetSHA())
				runsToRerun = append(runsToRerun, run)
				break
			}
		}
	}

	for _, run := range runsToRerun {
		if run.GetStatus() != "completed" {
			// Cancel non-completed runs before queuing a rerun.
			h.Debugf("Run %v status: %s", run.GetID(), run.GetStatus())
			_, err := h.Actions.CancelWorkflowRunByID(ctx, repoOwner, repoName, run.GetID())
			if err != nil {
				h.Debugf("Failed to cancel workflow run: %v", err)
			}
		} else if run.GetConclusion() == "success" {
			// Skip runs that have completed and succeeded, since they cannot be re-run.
			// This is still being worked on server-side afaik.
			h.Debugf("Workflow run %d succeeded, will not rerun", run.GetID())
			continue
		}

		h.Debugf("Rerunning %d", run.GetID())
		_, err := h.Actions.RerunWorkflowByID(ctx, repoOwner, repoName, run.GetID())
		if err != nil {
			h.Errorf("Failed to rerun workflow: %v", err)
		}
	}

	return nil
}

func (h *handler) getIssueForComment(ctx context.Context, comment *github.IssueComment) (issue *github.Issue, resp *github.Response, err error) {
	h.Debugf("Issue URL: %s", comment.GetIssueURL())
	req, err := h.NewRequest(http.MethodGet, comment.GetIssueURL(), nil)
	if err != nil {
		return nil, nil, fmt.Errorf("create request: %v", err)
	}
	issue = &github.Issue{}
	if resp, err = h.Do(ctx, req, issue); err != nil {
		return nil, resp, fmt.Errorf("do request: %v", err)
	}
	return issue, resp, nil
}

func (h *handler) initFromInputs(ctx context.Context) {
	allowListStr := h.GetInput("allow_user_regexp_list")
	denyListStr := h.GetInput("deny_user_regexp_list")

	var allowList, denyList []string
	if allowListStr != "" {
		if err := json.Unmarshal([]byte(allowListStr), &allowList); err != nil {
			h.Fatalf("Failed to unmarshal allow list")
		}
	}
	if denyListStr != "" {
		h.Debugf("Unmarshal deny: %s", denyListStr)
		if err := json.Unmarshal([]byte(denyListStr), &denyList); err != nil {
			h.Fatalf("Failed to unmarshal deny list")
		}
	}

	for _, reStr := range allowList {
		re, err := regexp.Compile(reStr)
		if err != nil {
			h.Fatalf("Failed to parse allow user regexp")
		}
		h.allowUserRegexps = append(h.allowUserRegexps, re)
	}
	for _, reStr := range denyList {
		h.Debugf("Parsing deny: %s", reStr)
		re, err := regexp.Compile(reStr)
		if err != nil {
			h.Fatalf("Failed to parse deny user regexp")
		}
		h.denyUserRegexps = append(h.denyUserRegexps, re)
	}

	token := h.GetInput("repo_token")
	if token == "" {
		h.Fatalf("Empty repo_token")
	}
	tc := oauth2.NewClient(ctx, oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	))

	h.Client = github.NewClient(tc)
}

func isIssueRerunable(issue *github.Issue) bool {
	// Only handle non-locked pull requests.
	return issue.IsPullRequest() && !issue.GetLocked()
}

func hasOkToTestLabel(issue *github.Issue) bool {
	// Gate reruns on "ok-to-test" label presence.
	for _, label := range issue.Labels {
		if label.GetName() == "ok-to-test" {
			return true
		}
	}
	return false
}

func (h handler) canUserComment(user string) bool {
	for _, allowRE := range h.allowUserRegexps {
		if !allowRE.MatchString(user) {
			return false
		}
	}
	for _, denyRE := range h.denyUserRegexps {
		if denyRE.MatchString(user) {
			return false
		}
	}
	return true
}

func parseCommentsToWorkflowNames(commentBody string) map[string]struct{} {
	// TODO: memoize some common strings on a per-repo basis, depending on performance of loading memoized strings.
	testsToRerun := make(map[string]struct{})
	scanner := bufio.NewScanner(strings.NewReader(commentBody))
	for scanner.Scan() {
		var splitComment []string
		for _, word := range strings.Split(scanner.Text(), " ") {
			if word = strings.TrimSpace(word); word != "" {
				splitComment = append(splitComment, word)
			}
		}
		// Ignore non-command comments or comments smaller than any command size.
		if len(splitComment) == 0 || len(splitComment[0]) < 5 || splitComment[0][0] != '/' {
			return nil
		}
		switch splitComment[0][1:] {
		case "retest":
			testsToRerun[testAll] = struct{}{}
		case "test":
			if len(splitComment) < 2 {
				continue
			}
			testsToRerun[splitComment[1]] = struct{}{}
		}
	}
	return testsToRerun
}
