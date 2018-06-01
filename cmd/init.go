package cmd

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	githubclient "github.com/google/go-github/github"

	"github.com/buildkite/cli/pkg"
	"github.com/buildkite/cli/pkg/graphql"
	"github.com/fatih/color"
)

const defaultPipelineYAML = `# Default pipeline from cli
steps:
- label: Example Test
  command: echo "Hello!"
`

type InitCommandContext struct {
	TerminalContext
	KeyringContext

	Debug     bool
	DebugHTTP bool
	Dir       string
}

func InitCommand(ctx InitCommandContext) error {
	dir, err := filepath.Abs(ctx.Dir)
	if err != nil {
		return NewExitError(err, 1)
	}

	pipelineFileTry := ctx.Try()
	pipelineFileTry.Start("Checking for pipeline file")

	pipelineFile := filepath.Join(dir, ".buildkite", "pipeline.yml")
	pipelineFileAdded := false

	// make sure we've got the directory in place for .buildkite/
	_ = os.Mkdir(filepath.Dir(pipelineFile), 0770)

	// create a .buildkite/pipeline.yml if one doesn't exist
	if _, err := os.Stat(pipelineFile); err == nil {
		pipelineFileTry.Success(".buildkite/pipeline.yml")
	} else {
		if err = ioutil.WriteFile(pipelineFile, []byte(defaultPipelineYAML), 0660); err != nil {
			return NewExitError(err, 1)
		}
		pipelineFileAdded = true
		pipelineFileTry.Success("Created .buildkite/pipeline.yml")
	}

	gitDir := filepath.Join(dir, ".git")

	// check we have a git directory
	if _, err := os.Stat(gitDir); err != nil {
		return NewExitError(fmt.Errorf("%s isn't a git managed project! Try `git init`", dir), 1)
	}

	debugf("[init] Examining git dir %s", gitDir)

	gitRepoTry := ctx.Try()
	gitRepoTry.Start("Checking for git repository and remote")

	// get the remote url, e.g git@github.com:buildkite/cli.git
	cmd := exec.Command("git", "--git-dir", gitDir, "remote", "get-url", "origin")
	output, err := cmd.CombinedOutput()

	if err != nil {
		gitRepoTry.Failure(err.Error())
		return NewExitError(err, 1)
	}

	gitRepoTry.Success(strings.TrimSpace(string(output)))

	u, err := pkg.ParseGittableURL(strings.TrimSpace(string(output)))
	if err != nil {
		gitRepoTry.Failure(fmt.Sprintf("Error parsing git remote: %v", err))
		return NewExitError(err, 1)
	}

	debugf("[init] Parsed %q as %#v", output, u)

	pathParts := strings.SplitN(strings.TrimLeft(strings.TrimSuffix(u.Path, ".git"), "/"), "/", 2)
	org := pathParts[0]
	repo := pathParts[1]

	debugf("[init] Detected a github remote for %s/%s", org, repo)

	bk, err := ctx.BuildkiteGraphQLClient()
	if err != nil {
		gitRepoTry.Failure(err.Error())
		return NewExitError(err, 1)
	}

	pipelineTry := ctx.Try()
	pipelineTry.Start("Checking for buildkite pipeline")

	isPipelineCreated, err := isBuildkitePipelineCreated(bk, org, repo)
	if err != nil {
		pipelineTry.Failure(err.Error())
		return NewExitError(err, 1)
	}

	var pipeline buildkitePipelineDetails

	if isPipelineCreated {
		var err error
		pipeline, err = getBuildkitePipeline(bk, org, repo)
		if err != nil {
			pipelineTry.Failure(err.Error())
			return NewExitError(err, 1)
		}

		pipelineTry.Success(pipeline.URL)
	} else {
		debugf("[init] Buildkite pipeline %s/%s doesn't exist", org, repo)

		var err error
		pipeline, err = createBuildkitePipeline(bk, org, repo,
			"steps:\n  - command: 'test.sh'",
			"git@github.com:foo/bar.git",
		)
		if err != nil {
			pipelineTry.Failure(err.Error())
			return NewExitError(err, 1)
		}

		pipelineTry.Success("Created " + pipeline.URL)
	}

	gh, err := ctx.GithubClient()
	if err != nil {
		return NewExitError(err, 1)
	}

	githubWebhooksTry := ctx.Try()
	githubWebhooksTry.Start(fmt.Sprintf("Checking github repository config for %s/%s", org, repo))

	hooks, _, err := gh.Repositories.ListHooks(context.Background(), org, repo, &githubclient.ListOptions{})
	if err != nil {
		githubWebhooksTry.Failure(err.Error())
		return NewExitError(err, 1)
	}

	isGithubWebhookSetup := false

	debugf("[init] Found %d webhooks", len(hooks))

	for _, hook := range hooks {
		wehookURL, ok := hook.Config["url"].(string)
		if ok && strings.Contains(wehookURL, "webhook.buildbox.io") || strings.Contains(wehookURL, "webhook.buildkite.com") {
			isGithubWebhookSetup = true
			break
		}
	}

	if isGithubWebhookSetup {
		githubWebhooksTry.Success("Webhook exists")
	} else {
		debugf("[init] Creating a webhook with %s", pipeline.WebhookURL)

		// https://developer.github.com/v3/repos/hooks/#create-a-hook
		_, _, err := gh.Repositories.CreateHook(context.Background(), org, repo, &githubclient.Hook{
			Name:   githubclient.String(`web`),
			Events: []string{`push`, `pull_request`, `deployment`},
			Config: map[string]interface{}{
				"url":          pipeline.WebhookURL,
				"content_type": "json",
			},
		})
		if err != nil {
			githubWebhooksTry.Failure(err.Error())
			return NewExitError(err, 1)
		}

		githubWebhooksTry.Success("Created webhook")
	}

	if pipelineFileAdded {
		ctx.Println("\nA pipeline.yml file was created in .buildkite, " +
			"you will need to manually commit this with " +
			"`git commit .buildkite -m 'Buildkite skeleton'")
	}

	ctx.Printf(color.GreenString("\nOk! Your project is ready to go at %s 🚀\n"), pipeline.URL)
	return nil
}

func getBuildkiteOrgID(client *graphql.Client, orgSlug string) (string, error) {
	resp, err := client.Do(`
		query($slug:ID!) {
			organization(slug: $slug) {
				id
			}
		}
	`, map[string]interface{}{
		"slug": orgSlug,
	})
	if err != nil {
		return "", err
	}

	var organizationQueryResponse struct {
		Data struct {
			Organization struct {
				ID string `json:"id"`
			} `json:"organization"`
		} `json:"data"`
	}

	if err = resp.DecodeInto(&organizationQueryResponse); err != nil {
		return "", fmt.Errorf("Failed to parse GraphQL response: %v", err)
	}

	if organizationQueryResponse.Data.Organization.ID == "" {
		return "", fmt.Errorf("Failed to find organization id for slug %q", orgSlug)
	}

	return organizationQueryResponse.Data.Organization.ID, nil
}

func isBuildkitePipelineCreated(client *graphql.Client, org, pipeline string) (bool, error) {
	resp, err := client.Do(`
		query($slug:ID!) {
			pipeline(slug: $slug) {
				repository {
					url
				}
			}
		}
	`, map[string]interface{}{
		"slug": fmt.Sprintf("%s/%s", org, pipeline),
	})
	if err != nil {
		return false, err
	}

	var pipelineQueryResponse struct {
		Data struct {
			Pipeline map[string]interface{} `json:"pipeline"`
		} `json:"data"`
	}

	if err = resp.DecodeInto(&pipelineQueryResponse); err != nil {
		return false, fmt.Errorf("Failed to parse GraphQL response: %v", err)
	}

	if len(pipelineQueryResponse.Data.Pipeline) > 0 {
		return true, nil
	}

	return false, nil
}

type buildkitePipelineDetails struct {
	Name       string
	Slug       string
	URL        string
	WebhookURL string
}

func createBuildkitePipeline(client *graphql.Client, org, pipeline, steps, repository string) (buildkitePipelineDetails, error) {
	orgId, err := getBuildkiteOrgID(client, org)
	if err != nil {
		return buildkitePipelineDetails{}, err
	}

	resp, err := client.Do(`
		mutation($input:PipelineCreateInput!) {
			pipelineCreate(input:$input) {
				pipeline {
					name
					slug
					url
					repository {
						provider {
							webhookUrl
						}
					}
				}
			}
		}
	`, map[string]interface{}{
		"input": map[string]interface{}{
			"name":           pipeline,
			"organizationId": orgId,
			"steps":          map[string]interface{}{"yaml": steps},
			"repository":     map[string]interface{}{"url": repository},
		}})
	if err != nil {
		return buildkitePipelineDetails{}, err
	}

	var parsedResp struct {
		Data struct {
			PipelineCreate struct {
				Pipeline struct {
					Name       string `json:"name"`
					Slug       string `json:"slug"`
					URL        string `json:"url"`
					Repository struct {
						Provider struct {
							WebhookURL string `json:"webhookUrl"`
						} `json:"provider"`
					} `json:"repository"`
				} `json:"pipeline"`
			} `json:"pipelineCreate"`
		} `json:"data"`
	}

	if err = resp.DecodeInto(&parsedResp); err != nil {
		return buildkitePipelineDetails{},
			fmt.Errorf("Failed to parse GraphQL response: %v", err)
	}

	return buildkitePipelineDetails{
		Name:       parsedResp.Data.PipelineCreate.Pipeline.Name,
		Slug:       parsedResp.Data.PipelineCreate.Pipeline.Slug,
		URL:        parsedResp.Data.PipelineCreate.Pipeline.URL,
		WebhookURL: parsedResp.Data.PipelineCreate.Pipeline.Repository.Provider.WebhookURL,
	}, nil
}

func getBuildkitePipeline(client *graphql.Client, org, pipeline string) (buildkitePipelineDetails, error) {
	resp, err := client.Do(`
		query($slug:ID!) {
			pipeline(slug: $slug) {
				name
				slug
				url
				repository {
					provider {
						webhookUrl
					}
				}
			}
		}
	`, map[string]interface{}{
		"slug": fmt.Sprintf("%s/%s", org, pipeline),
	})
	if err != nil {
		return buildkitePipelineDetails{}, err
	}

	var parsedResp struct {
		Data struct {
			Pipeline struct {
				Name       string `json:"name"`
				Slug       string `json:"slug"`
				URL        string `json:"url"`
				Repository struct {
					Provider struct {
						WebhookURL string `json:"webhookUrl"`
					} `json:"provider"`
				} `json:"repository"`
			} `json:"pipeline"`
		} `json:"data"`
	}

	if err = resp.DecodeInto(&parsedResp); err != nil {
		return buildkitePipelineDetails{}, fmt.Errorf("Failed to parse GraphQL response: %v", err)
	}

	return buildkitePipelineDetails{
		Name:       parsedResp.Data.Pipeline.Name,
		Slug:       parsedResp.Data.Pipeline.Slug,
		URL:        parsedResp.Data.Pipeline.URL,
		WebhookURL: parsedResp.Data.Pipeline.Repository.Provider.WebhookURL,
	}, nil
}
