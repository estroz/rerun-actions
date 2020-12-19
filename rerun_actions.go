package main

import (
	"bufio"
	"context"
	"encoding/json"
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

func (h *handler) handle(ctx context.Context, repoOwner, repoName string, eventID int64) error {
	event, _, err := h.Issues.GetEvent(ctx, repoOwner, repoName, eventID)
	if err != nil {
		h.Errorf("Failed to get issue event: %v", err)
		return nil
	}

	// Only handle new comments.
	if event.GetEvent() != "commented" {
		h.Debugf("Event: %s", event.GetEvent())
		return nil
	}

	issue := event.GetIssue()

	if !isIssueRerunable(issue) {
		return nil
	}

	// Check allow and deny lists if present.
	actor := event.GetActor().GetLogin()
	if !h.canActorComment(actor) {
		h.Debugf("Issue comment was created by ignored user %s", actor)
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
		return nil
	}

	body := issue.GetBody()
	testsToRerun := parseCommentsToWorkflowNames(body)

	opts := &github.ListOptions{}
	allWorkflows, _, err := h.Actions.ListWorkflows(ctx, repoOwner, repoName, opts)
	if err != nil {
		h.Errorf("Failed to list workflows: %v", err)
		return nil
	}

	var workflows []*github.Workflow
	if _, rerunAll := testsToRerun[testAll]; rerunAll {
		workflows = allWorkflows.Workflows
	} else {
		for _, workflow := range allWorkflows.Workflows {
			if _, hasWorkflow := testsToRerun[workflow.GetName()]; hasWorkflow {
				workflows = append(workflows, workflow)
			}
		}
	}

	var runsToRerun []*github.WorkflowRun
	for _, workflow := range workflows {
		h.Debugf("Workflow name: %s (%s)", workflow.GetName(), workflow.GetPath())
		// Do not attempt to rerun inactive workflows.
		if workflow.GetState() != "active" {
			continue
		}
		opts := &github.ListWorkflowRunsOptions{
			// Filter by whoever created the PR.
			Actor: issue.GetUser().GetLogin(),
			// Filter on pull request runs.
			Event: "pull_request",
		}
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
				runsToRerun = append(runsToRerun, run)
				break
			}
		}
	}

	for _, run := range runsToRerun {
		// Cancel non-completed runs before queuing a rerun.
		if run.GetStatus() != "completed" {
			h.Debugf("Run %v status: %s", run.GetID(), run.GetStatus())
			_, err := h.Actions.CancelWorkflowRunByID(ctx, repoOwner, repoName, run.GetID())
			if err != nil {
				h.Debugf("Failed to cancel workflow run: %v", err)
			}
		}
		// QUESTION: will this fail if workflow run was successful?
		_, err := h.Actions.RerunWorkflowByID(ctx, repoOwner, repoName, run.GetID())
		if err != nil {
			h.Errorf("Failed to rerun workflow: %v", err)
		}
	}

	return nil
}

func (h *handler) initFromInputs(ctx context.Context) {
	allowListStr := h.Action.GetInput("allow-user-regexp-list")
	denyListStr := h.Action.GetInput("deny-user-regexp-list")

	var allowList, denyList []string
	if allowListStr != "" {
		if err := json.Unmarshal([]byte(allowListStr), &allowList); err != nil {
			h.Action.Fatalf("Failed to unmarshal allow list")
		}
	}
	if denyListStr != "" {
		if err := json.Unmarshal([]byte(denyListStr), &denyList); err != nil {
			h.Action.Fatalf("Failed to unmarshal deny list")
		}
	}

	for _, reStr := range allowList {
		re, err := regexp.Compile(reStr)
		if err != nil {
			h.Action.Fatalf("Failed to parse allow user regexp")
		}
		h.allowUserRegexps = append(h.allowUserRegexps, re)
	}
	for _, reStr := range denyList {
		h.Action.Debugf("Parsing deny: %s", reStr)
		re, err := regexp.Compile(reStr)
		if err != nil {
			h.Action.Fatalf("Failed to parse deny user regexp")
		}
		h.denyUserRegexps = append(h.denyUserRegexps, re)
	}

	token := h.Action.GetInput("repo-token")
	if token == "" {
		h.Action.Fatalf("Empty token")
	}
	tc := oauth2.NewClient(ctx, oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	))

	h.Client = github.NewClient(tc)
}

func isIssueRerunable(issue *github.Issue) bool {
	// Only handle non-locked pull requests.
	if !issue.IsPullRequest() || issue.GetLocked() {
		return false
	}

	// Gate reruns on "ok-to-test" label presence.
	for _, label := range issue.Labels {
		if label.String() == "ok-to-test" {
			return true
		}
	}

	return true
}

func (h handler) canActorComment(actor string) bool {
	for _, allowRE := range h.allowUserRegexps {
		if !allowRE.MatchString(actor) {
			return false
		}
	}
	for _, denyRE := range h.denyUserRegexps {
		if denyRE.MatchString(actor) {
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
