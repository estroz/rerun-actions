# rerun-actions

A GitHub Action that re-runs other Action Workflows via PR comment commands.

## How it works

Once a PR is submitted against a repo with GitHub Actions enabled, a workflow
will run. If that workflow fails, it can be rerun by `POST`-ing to the Actions API.
`rerun-actions` reruns workflows by parsing the body of a PR comment (specified by the comment's id
retrieved from an [`issue_comment` webhook event][issue_comment_wh]) for certain [commands](#comment-commands)
and rerunning jobs matching those commands.

Some notes:
- The PR either must have an `ok-to-test` label present on the PR, or the user who writes a command must
be an organization member, or repo owner, contributor, or collaborator.
  - Typically `ok-to-test` can/should only be applied by repo reviewers/approvers to prevent spam and abuse.
- `rerun-actions` should only be run on comment creation. See the below [examples](#examples) for how to do this.

## Comment commands

These commands are named after

- `/retest` - rerun all failed workflows.
- `/test <workflow name>` - rerun a specific failed workflow. Only one workflow name can be specified.
Multiple `/test` commands are allowed per comment.

Caveat: because of Action limitations, only failed workflows can be rerun.

## Examples

Example workflow file (use this config verbatim):

```yaml
on:
  issue_comment:
    types: [created]

jobs:
  rerun_pr_tests:
    name: rerun_pr_tests
    if: ${{ github.event.issue.pull_request }}
    runs-on: ubuntu-18.04
    steps:
    - uses: estroz/rerun-actions@main
      with:
        repo_token: ${{ secrets.GITHUB_TOKEN }}
        comment_id: ${{ github.event.comment.id }}
```

[issue_comment_wh]:https://docs.github.com/en/free-pro-team@latest/developers/webhooks-and-events/webhook-events-and-payloads#issue_comment
