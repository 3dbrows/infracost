package main

import (
	"fmt"
	"github.com/infracost/infracost/internal/apiclient"
	"github.com/infracost/infracost/internal/comment"
	"github.com/infracost/infracost/internal/config"
	"github.com/infracost/infracost/internal/output"
	"github.com/infracost/infracost/internal/ui"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"strconv"
	"strings"
)

var validCommentGitLabBehaviors = []string{"update", "new", "delete-and-new"}

func commentGitLabCmd(ctx *config.RunContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gitlab",
		Short: "Post an Infracost comment to GitLab",
		Long:  "Post an Infracost comment to GitLab",
		Example: `  Update a comment on a merge request:

      infracost comment gitlab --repo my-org/my-gitlab-repo --merge-request 3 --path infracost.json --gitlab-token $GITLAB_TOKEN

  Post a new comment to a commit:

      infracost comment gitlab --repo my-org/my-gitlab-repo --commit 2ca7182 --path infracost.json --behavior delete-and-new --gitlab-token $GITLAB_TOKEN`,
		ValidArgs: []string{"--", "-"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx.SetContextValue("platform", "gitlab")

			var err error

			serverURL, _ := cmd.Flags().GetString("gitlab-server-url")
			token, _ := cmd.Flags().GetString("gitlab-token")
			tag, _ := cmd.Flags().GetString("tag")
			extra := comment.GitLabExtra{
				ServerURL: serverURL,
				Token:     token,
				Tag:       tag,
			}

			commit, _ := cmd.Flags().GetString("commit")
			mrNumber, _ := cmd.Flags().GetInt("merge-request")
			repo, _ := cmd.Flags().GetString("repo")

			var commentHandler *comment.CommentHandler
			if mrNumber != 0 {
				ctx.SetContextValue("targetType", "merge-request")

				commentHandler, err = comment.NewGitLabPRHandler(ctx.Context(), repo, strconv.Itoa(mrNumber), extra)
				if err != nil {
					return err
				}
			} else if commit != "" {
				ctx.SetContextValue("targetType", "commit")

				commentHandler, err = comment.NewGitLabCommitHandler(ctx.Context(), repo, commit, extra)
				if err != nil {
					return err
				}
			} else {
				ui.PrintUsage(cmd)
				return fmt.Errorf("either --commit or --merge-request is required")
			}

			behavior, _ := cmd.Flags().GetString("behavior")
			if behavior != "" && !contains(validCommentGitLabBehaviors, behavior) {
				ui.PrintUsage(cmd)
				return fmt.Errorf("--behavior only supports %s", strings.Join(validCommentGitLabBehaviors, ", "))
			}
			ctx.SetContextValue("behavior", behavior)

			paths, _ := cmd.Flags().GetStringArray("path")

			body, err := buildCommentBody(ctx, paths, output.MarkdownOptions{
				WillUpdate:          mrNumber != 0 && behavior == "update",
				WillReplace:         mrNumber != 0 && behavior == "delete-and-new",
				IncludeFeedbackLink: true,
			})
			if err != nil {
				return err
			}

			dryRun, _ := cmd.Flags().GetBool("dry-run")
			if !dryRun {
				err = commentHandler.CommentWithBehavior(ctx.Context(), behavior, string(body))
				if err != nil {
					return err
				}

				pricingClient := apiclient.NewPricingAPIClient(ctx)
				err = pricingClient.AddEvent("infracost-comment", ctx.EventEnv())
				if err != nil {
					log.Errorf("Error reporting event: %s", err)
				}

				cmd.Println("Comment posted to GitLab")
			} else {
				cmd.Println(string(body))
				cmd.Println("Comment not posted to GitLab (--dry-run was specified)")
			}

			return nil
		},
	}

	cmd.Flags().String("behavior", "update", `Behavior when posting the comment, one of:
  update (default)  Update the latest comment
  new               Create a new comment
  delete-and-new    Delete previous matching comments and create a new comment`)
	_ = cmd.RegisterFlagCompletionFunc("behavior", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return validCommentGitLabBehaviors, cobra.ShellCompDirectiveDefault
	})
	cmd.Flags().String("commit", "", "Commit SHA to post/get the comment, mutually exclusive with merge-request")
	cmd.Flags().String("gitlab-server-url", "https://gitlab.com", "GitLab Server URL, defaults to https://gitlab.com")
	cmd.Flags().String("gitlab-token", "", "GitLab token")
	_ = cmd.MarkFlagRequired("gitlab-token")
	cmd.Flags().StringArrayP("path", "p", []string{}, "Path to Infracost JSON files, glob patterns need quotes")
	_ = cmd.MarkFlagRequired("path")
	_ = cmd.MarkFlagFilename("path", "json")
	cmd.Flags().Int("merge-request", 0, "Merge request number to post the comment on, mutually exclusive with commit")
	cmd.Flags().String("repo", "", "Repository in the format owner/repo")
	_ = cmd.MarkFlagRequired("repo")
	cmd.Flags().String("tag", "", "Customize the embedded tag that is used for detecting comments posted by Infracost")
	cmd.Flags().Bool("dry-run", false, "Generate the comment without actually posting to GitLab.")

	return cmd
}
