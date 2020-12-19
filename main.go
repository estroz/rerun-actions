package main

import (
	"context"
	"os"
	"path"
	"strconv"

	actions "github.com/sethvargo/go-githubactions"
)

func main() {

	h := &handler{
		Action: actions.New(),
	}

	ctx := context.Background()
	h.initFromInputs(ctx)

	eventIDStr := h.GetInput("event-id")
	if eventIDStr == "" {
		h.Action.Fatalf("Empty event-id")
	}
	eventID, err := strconv.ParseInt(eventIDStr, 10, 64)
	if err != nil {
		h.Fatalf("%v", err)
	}

	repo := os.Getenv("GITHUB_REPOSITORY")
	if repo == "" {
		h.Action.Fatalf("Empty repo")
	}
	repoOwner, repoName := path.Split(repo)
	if err := h.handle(ctx, repoOwner, repoName, eventID); err != nil {
		h.Fatalf("%v", err)
	}
}
