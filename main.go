package main

import (
	"context"
	"os"
	"path"
	"strconv"
	"strings"

	actions "github.com/sethvargo/go-githubactions"
)

func main() {

	h := &handler{
		Action: actions.New(),
	}

	ctx := context.Background()
	h.initFromActionsEnv(ctx)

	commentIDStr := h.GetInput("comment_id")
	if commentIDStr == "" {
		h.Fatalf("Empty comment_id")
	}
	commentID, err := strconv.ParseInt(commentIDStr, 10, 64)
	if err != nil {
		h.Fatalf("Failed to parse comment_id: %v", err)
	}

	repo := os.Getenv("GITHUB_REPOSITORY")
	if repo == "" {
		h.Fatalf("GITHUB_REPOSITORY not set")
	}
	repoOwner, repoName := path.Split(repo)
	repoOwner = strings.Trim(repoOwner, "/")
	h.Debugf("Repo owner=%s name=%s commentID=%d", repoOwner, repoName, commentID)
	if err := h.handle(ctx, repoOwner, repoName, commentID); err != nil {
		h.Fatalf("%v", err)
	}
}
