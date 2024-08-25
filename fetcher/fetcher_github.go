package fetcher

import (
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
	// internal state
	delay         bool
	latestRelease struct {
		UpdatedAt time.Time
	}
	githubClient *github.Client
	httpClient   *http.Client
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

	release, resp, err := h.githubClient.Repositories.GetLatestRelease(h.Context, h.User, h.Repo)
	if err != nil {
		return nil, fmt.Errorf("failed to get last release: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to get last release: %s", resp.Status)
	}

	for _, asset := range release.Assets {
		if h.Match(asset.GetName()) {
			if h.latestRelease.UpdatedAt == asset.UpdatedAt.Time {
				return nil, errors.New("no new release")
			}
			body, _, err := h.githubClient.Repositories.DownloadReleaseAsset(h.Context, h.User, h.Repo, asset.GetID(), h.httpClient)
			if err != nil {
				return nil, fmt.Errorf("failed to download release asset: %w", err)
			}
			h.latestRelease.UpdatedAt = asset.UpdatedAt.Time
			return body, nil
		}
	}

	return nil, nil
}
