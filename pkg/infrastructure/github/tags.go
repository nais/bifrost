package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	githubApiUrl     = "https://api.github.com"
	unleashRepoOwner = "nais"
	unleashRepoName  = "unleash"
)

const (
	// githubRequestTimeout bounds a single tags request so a slow/hung GitHub
	// connection cannot block the caller (and its request handler) indefinitely.
	githubRequestTimeout = 10 * time.Second
	// githubMaxResponseBytes caps how much of the response we read, guarding
	// against an unexpectedly large body.
	githubMaxResponseBytes = 5 << 20 // 5 MiB
	// versionsCacheTTL is how long fetched versions are cached, so we do not hit
	// GitHub (and its 60 req/hr unauthenticated rate limit) on every call.
	versionsCacheTTL = 5 * time.Minute
)

var httpClient = &http.Client{Timeout: githubRequestTimeout}

func getLatestTags(ctx context.Context, owner, repo string) ([]string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/tags?per_page=100", githubApiUrl, owner, repo)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	// Authenticate when a token is available to raise the rate limit from 60 to
	// 5000 requests/hour, which matters behind shared cluster egress NAT.
	if token := os.Getenv("BIFROST_GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var tags []struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, githubMaxResponseBytes)).Decode(&tags); err != nil {
		return nil, err
	}

	tagNames := make([]string, 0, len(tags))
	for _, tag := range tags {
		tagNames = append(tagNames, tag.Name)
	}

	return tagNames, nil
}

func tagToUnleashVersion(tag string) (UnleashVersion, error) {
	tagValidator := regexp.MustCompile(`^v\d+\.\d+\.\d+-\d{8}-\d{6}-\w{7}$`)
	if !tagValidator.MatchString(tag) {
		return UnleashVersion{}, fmt.Errorf("invalid tag: %s", tag)
	}

	// Split the tag into its components
	tagComponents := strings.Split(tag, "-")

	// Parse the version number
	versionComponents := strings.Split(tagComponents[0], "v")
	versionNumber := versionComponents[1]

	// Parse the release date and time
	releaseDateTime, err := time.Parse("20060102-150405", fmt.Sprintf("%s-%s", tagComponents[1], tagComponents[2]))
	if err != nil {
		return UnleashVersion{}, fmt.Errorf("invalid release date/time: %s-%s", tagComponents[1], tagComponents[2])
	}

	// Parse the commit hash
	commitHash := tagComponents[3]

	return UnleashVersion{
		VersionNumber: versionNumber,
		ReleaseTime:   releaseDateTime,
		CommitHash:    commitHash,
		GitTag:        tag,
	}, nil
}

type UnleashVersion struct {
	VersionNumber string
	ReleaseTime   time.Time
	CommitHash    string
	GitTag        string
}

var (
	versionsCacheMu   sync.Mutex
	versionsCache     []UnleashVersion
	versionsCacheTime time.Time
)

// UnleashVersions returns the known Unleash image versions, newest first.
// Results are cached for versionsCacheTTL to avoid hammering the GitHub API.
func UnleashVersions() ([]UnleashVersion, error) {
	versionsCacheMu.Lock()
	defer versionsCacheMu.Unlock()

	if versionsCache != nil && time.Since(versionsCacheTime) < versionsCacheTTL {
		return versionsCache, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), githubRequestTimeout)
	defer cancel()

	tags, err := getLatestTags(ctx, unleashRepoOwner, unleashRepoName)
	if err != nil {
		return nil, err
	}

	versions := make([]UnleashVersion, 0, len(tags))
	for _, tag := range tags {
		version, err := tagToUnleashVersion(tag)
		if err == nil {
			versions = append(versions, version)
		}
	}

	// Sort newest first by release time so callers that take index 0 get the
	// latest release, independent of GitHub's tag ordering.
	sort.Slice(versions, func(i, j int) bool {
		return versions[i].ReleaseTime.After(versions[j].ReleaseTime)
	})

	versionsCache = versions
	versionsCacheTime = time.Now()

	return versions, nil
}
