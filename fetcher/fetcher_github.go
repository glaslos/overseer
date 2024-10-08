package fetcher

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/google/go-github/v64/github"
)

// Github uses the Github V3 API to retrieve the latest release of a given repository and enumerate its assets. If a release
// contains a matching asset, it will fetch and return its io.Reader stream.
type Github struct {
	// Github username and repository name
	User, Repo string
	// Token is optional for authenticated requests (private repos)
	Token string
	// Interval between fetches
	Interval time.Duration
	// Match is used to find matching release asset.
	// By default a file will match if it contains both GOOS and GOARCH.
	Match   func(filename string) bool
	Context context.Context
	// Fetch latest artifact instead of release
	Artifact bool
	// internal state
	delay         bool
	latestRelease time.Time
	latestRun     int64
	githubClient  *github.Client
	httpClient    *http.Client
}

func (h *Github) defaultAsset(filename string) bool {
	return strings.Contains(filename, runtime.GOOS) && strings.Contains(filename, runtime.GOARCH)
}

// Init validates the provided config
func (h *Github) Init() error {
	//apply defaults
	if h.User == "" {
		return errors.New("user required")
	}
	if h.Repo == "" {
		return errors.New("repo required")
	}
	if h.Match == nil {
		h.Match = h.defaultAsset
	}

	if h.Interval < time.Minute {
		h.Interval = time.Minute
	}

	if h.Context == nil {
		h.Context = context.Background()
	}

	h.httpClient = &http.Client{Timeout: time.Minute}
	h.githubClient = github.NewClient(h.httpClient).WithAuthToken(h.Token)
	return nil
}

// Fetch the binary from the provided Repository
func (h *Github) Fetch() (io.Reader, error) {
	// delay fetches after first
	if h.delay {
		time.Sleep(h.Interval)
	}

	h.delay = true

	if h.Artifact {
		return h.fetchLatestArtifact()
	}
	return h.fetchLatestRelease()
}

func (h *Github) fetchLatestRelease() (io.Reader, error) {
	release, resp, err := h.githubClient.Repositories.GetLatestRelease(h.Context, h.User, h.Repo)
	if resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get last release: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to get last release: %s", resp.Status)
	}

	for _, asset := range release.Assets {
		if h.Match(asset.GetName()) {
			if h.latestRelease == asset.UpdatedAt.Time {
				return nil, errors.New("no new release")
			}
			body, _, err := h.githubClient.Repositories.DownloadReleaseAsset(h.Context, h.User, h.Repo, asset.GetID(), h.httpClient)
			if err != nil {
				return nil, fmt.Errorf("failed to download release asset: %w", err)
			}
			h.latestRelease = asset.UpdatedAt.Time
			return body, nil
		}
	}
	return nil, nil
}

func (h *Github) fetchLatestArtifact() (io.Reader, error) {
	runs, resp, err := h.githubClient.Actions.ListRepositoryWorkflowRuns(h.Context, h.User, h.Repo, &github.ListWorkflowRunsOptions{
		Branch: "main",
		Status: "success",
		ListOptions: github.ListOptions{
			PerPage: 1,
		},
	})
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get workflow runs: %w", err)
	}
	if len(runs.WorkflowRuns) == 0 {
		return nil, errors.New("no successful workflow runs")
	}

	if h.latestRun == runs.WorkflowRuns[0].GetID() {
		return nil, errors.New("no new run")
	}

	artifacts, resp, err := h.githubClient.Actions.ListWorkflowRunArtifacts(h.Context, h.User, h.Repo, runs.WorkflowRuns[0].GetID(), nil)
	if resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get workflow run artifacts: %w", err)
	}
	if len(artifacts.Artifacts) == 0 {
		return nil, errors.New("no artifacts found")
	}
	for _, artifact := range artifacts.Artifacts {
		if h.Match(artifact.GetName()) {
			url, resp, err := h.githubClient.Actions.DownloadArtifact(h.Context, h.User, h.Repo, artifact.GetID(), 10)
			if err != nil {
				return nil, fmt.Errorf("failed to download artifact: %w", err)
			}
			if resp.Body != nil {
				defer resp.Body.Close()
			}

			if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusFound {
				return nil, fmt.Errorf("unexpected status code: %s", resp.Status)
			}

			req, err := http.NewRequest(http.MethodGet, url.String(), nil)
			if err != nil {
				return nil, fmt.Errorf("failed to create request: %w", err)
			}
			req.Header.Set("Accept", "application/vnd.github.v3+json")
			req.Header.Set("User-Agent", "actuated-batch")

			urlResp, err := h.httpClient.Do(req)
			if err != nil {
				return nil, fmt.Errorf("failed to download artifact: %w", err)
			}

			h.latestRun = runs.WorkflowRuns[0].GetID()
			body, err := io.ReadAll(urlResp.Body)
			if err != nil {
				return nil, fmt.Errorf("failed to read artifact body: %w", err)
			}
			reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
			if err != nil {
				return nil, fmt.Errorf("failed to read zip archive: %w", err)
			}
			if len(reader.File) == 0 {
				return nil, errors.New("no files in archive")
			}
			return reader.File[0].Open()
		}
	}
	return nil, nil
}
