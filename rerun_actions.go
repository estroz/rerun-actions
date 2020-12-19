package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/go-github/v33/github"
	"github.com/palantir/go-githubapp/githubapp"
)

const testAll = "__all"

type RerunActionsHandler struct {
	githubapp.ClientCreator

	allowUserRegexps []*regexp.Regexp
	denyUserRegexps  []*regexp.Regexp
}

func (h *RerunActionsHandler) Handles() []string {
	return []string{"issue_comment"}
}

func (h *RerunActionsHandler) Handle(ctx context.Context, eventType, deliveryID string, payload []byte) error {
	var event github.IssueCommentEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return fmt.Errorf("failed to parse issue comment event payload: %v", err)
	}

	// Only handle new comments.
	if event.GetAction() != "created" {
		return nil
	}

	issue := event.GetIssue()

	if !isIssueRerunable(issue) {
		return nil
	}

	repo := event.GetRepo()
	installationID := githubapp.GetInstallationIDFromEvent(&event)
	ctx, logger := githubapp.PrepareRepoContext(ctx, installationID, repo)
	client, err := h.NewInstallationClient(installationID)
	if err != nil {
		return err
	}

	// Check allow and deny lists if present.
	author := event.GetComment().GetUser().GetLogin()
	if !h.canAuthorComment(author) {
		logger.Debug().Msgf("Issue comment was created by ignored user %s", author)
		return nil
	}

	repoOwner := repo.GetOwner().GetLogin()
	repoName := repo.GetName()

	prNum := issue.GetNumber()
	pr, _, err := client.PullRequests.Get(ctx, repoOwner, repoName, prNum)
	if err != nil {
		logger.Error().Err(err).Msg("Failed to get PR")
		return nil
	}

	// Can't rerun actions on merged PRs.
	if pr.GetMerged() {
		return nil
	}

	// TODO: memoize some common strings on a per-repo basis, depending on performance of loading memoized strings.
	body := event.GetComment().GetBody()
	testsToRerun := parseCommentsToWorkflowNames(body)

	opts := &github.ListOptions{}
	allWorkflows, _, err := client.Actions.ListWorkflows(ctx, repoOwner, repoName, opts)
	if err != nil {
		logger.Error().Err(err).Msg("Failed to list workflows")
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
		workflowRuns, _, err := client.Actions.ListWorkflowRunsByID(ctx, repoOwner, repoName, workflow.GetID(), opts)
		if err != nil {
			logger.Error().Err(err).Msg("Failed to list workflow runs")
			return nil
		}
		for _, run := range workflowRuns.WorkflowRuns {
			// Stop searching runs once an older run is found (assuming they are linearized by time).
			if run.GetCreatedAt().Before(pr.GetCreatedAt()) {
				logger.Info().Msgf("Older workflow run than PR %d found", prNum)
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
			_, err := client.Actions.CancelWorkflowRunByID(ctx, repoOwner, repoName, run.GetID())
			if err != nil {
				logger.Debug().Msgf("Failed to cancel workflow run: %v", err)
			}
		}
		_, err := client.Actions.RerunWorkflowByID(ctx, repoOwner, repoName, run.GetID())
		if err != nil {
			logger.Error().Err(err).Msg("Failed to rerun workflow")
		}
	}

	return nil
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

func (h RerunActionsHandler) canAuthorComment(author string) bool {
	for _, allowRE := range h.allowUserRegexps {
		if !allowRE.MatchString(author) {
			return false
		}
	}
	for _, denyRE := range h.denyUserRegexps {
		if denyRE.MatchString(author) {
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
