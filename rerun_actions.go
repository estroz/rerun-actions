package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/google/go-github/v33/github"
	actions "github.com/sethvargo/go-githubactions"
	"golang.org/x/oauth2"
)

const (
	testAll              = "__all"
	completedStatus      = "completed"
	successfulConclusion = "success"

	canTestLabel              = "ok-to-test"
	retestAllWorkflowsCommand = "rerun-all"
	testWorkflowCommand       = "rerun-workflow"
)

type handler struct {
	*github.Client
	*actions.Action
}

// initFromActionsEnv initializes h from a GH Actions environment.
func (h *handler) initFromActionsEnv(ctx context.Context) {
	token := h.GetInput("repo_token")
	if token == "" {
		h.Fatalf("Empty repo_token")
	}
	h.Client = github.NewClient(oauth2.NewClient(ctx, oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)))
}

// handle reruns a set of actions for the PR associated with a given commentID, if possible.
func (h *handler) handle(ctx context.Context, repoOwner, repoName string, commentID int64) error {
	comment, _, err := h.Issues.GetComment(ctx, repoOwner, repoName, commentID)
	if err != nil {
		h.Errorf("Failed to get comment: %v", err)
		return nil
	}
	h.Debugf("Comment %d found", comment.GetID())

	// Reduce the number of API calls when a PR comment that does not contain a command is created
	// by returning if no commands are present in the comment body.
	testsToRerun := parseCommentsToWorkflowNames(comment.GetBody())
	if len(testsToRerun) == 0 {
		h.Debugf("No commands in comment body")
		return nil
	}

	issue, _, err := h.getIssueForComment(ctx, comment)
	if err != nil {
		h.Errorf("Failed to get issue: %v", err)
		return nil
	}
	h.Debugf("Issue %d found", issue.GetID())

	// Actions associated with non-PR issues and locked PRs cannot be rerun.
	if !isIssueRerunable(issue) {
		h.Debugf("Issue is not a PR or is locked")
		return nil
	}

	// Issue must have "ok-to-test" label, or the issue commenter must have org/repo permissions to run tests.
	if !hasOkToTestLabel(issue) && !isCommenterPrivileged(comment.GetAuthorAssociation()) {
		h.Debugf("Issue lacks the \"ok-to-test\" label (labels: %v) and commenter is unprivileged (association: %s)",
			issue.Labels, comment.GetAuthorAssociation())
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

	opts := &github.ListOptions{}
	allWorkflows, _, err := h.Actions.ListWorkflows(ctx, repoOwner, repoName, opts)
	if err != nil {
		h.Errorf("Failed to list workflows: %v", err)
		return nil
	}

	var workflows []*github.Workflow
	if _, rerunAll := testsToRerun[testAll]; rerunAll {
		h.Debugf("Rerunning all workflows")
		workflows = allWorkflows.Workflows
	} else {
		for _, workflow := range allWorkflows.Workflows {
			if _, hasWorkflow := testsToRerun[workflow.GetName()]; !hasWorkflow {
				h.Debugf("Workflow %s not found", workflow.GetName())
				continue
			}
			h.Debugf("Workflow %s found", workflow.GetName())
			workflows = append(workflows, workflow)
		}
	}

	var runsToRerun []*github.WorkflowRun
	for _, workflow := range workflows {
		h.Debugf("Workflow name: %s (%s)", workflow.GetName(), workflow.GetPath())
		// Always skip this workflow to prevent recursion issues.
		if wfName := os.Getenv("GITHUB_WORKFLOW"); wfName == workflow.GetName() || wfName == workflow.GetPath() {
			h.Debugf("Skipping the workflow containing this job")
			continue
		}
		// Do not attempt to rerun inactive workflows.
		if workflow.GetState() != "active" {
			h.Debugf("Skipping inactive workflow")
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
			// Stop searching runs once an older run is found.
			if run.GetCreatedAt().Before(pr.GetCreatedAt()) {
				h.Debugf("Older workflow run than PR %d found", prNum)
				break
			}
			// A matching run's SHA will match the PR's head SHA.
			if run.GetHeadSHA() == pr.GetHead().GetSHA() {
				h.Debugf("Found run matching PR %d SHA %s", prNum, pr.GetHead().GetSHA())
				runsToRerun = append(runsToRerun, run)
				break
			}
		}
	}

	for _, run := range runsToRerun {
		if run.GetStatus() == completedStatus && run.GetConclusion() == successfulConclusion {
			// Skip runs that have completed and succeeded, since they cannot be re-run.
			// This is still being worked on server-side afaik.
			h.Debugf("Workflow run %d succeeded, will not rerun", run.GetID())
			continue
		}
		if run.GetStatus() != completedStatus {
			// Cancel non-completed runs before queuing a rerun.
			h.Debugf("Cancellling %s run %v", run.GetStatus(), run.GetID())
			_, err := h.Actions.CancelWorkflowRunByID(ctx, repoOwner, repoName, run.GetID())
			if err != nil {
				h.Debugf("Failed to cancel workflow run: %v", err)
			}
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

func isIssueRerunable(issue *github.Issue) bool {
	// Only handle non-locked pull requests.
	return issue.IsPullRequest() && !issue.GetLocked()
}

func hasOkToTestLabel(issue *github.Issue) bool {
	// Gate reruns on "ok-to-test" label presence.
	for _, label := range issue.Labels {
		if label.GetName() == canTestLabel {
			return true
		}
	}
	return false
}

// From API docs:
// AuthorAssociation is the comment author's relationship to the issue's repository.
// Possible values are "COLLABORATOR", "CONTRIBUTOR", "FIRST_TIMER", "FIRST_TIME_CONTRIBUTOR", "MEMBER", "OWNER", or "NONE".
var privilegedAssociations = map[string]struct{}{
	"collaborator": {},
	"contributor":  {},
	"member":       {},
	"owner":        {},
}

// isCommenterPrivileged returns true if authorAssoc is a privileged keyword:
// "collaborator", "contributor", "member", or "owner".
func isCommenterPrivileged(authorAssoc string) bool {
	_, isPrivileged := privilegedAssociations[strings.ToLower(authorAssoc)]
	return isPrivileged
}

func parseCommentsToWorkflowNames(commentBody string) map[string]struct{} {
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
		case retestAllWorkflowsCommand:
			testsToRerun[testAll] = struct{}{}
		case testWorkflowCommand:
			if len(splitComment) < 2 {
				continue
			}
			testsToRerun[splitComment[1]] = struct{}{}
		}
	}
	return testsToRerun
}
