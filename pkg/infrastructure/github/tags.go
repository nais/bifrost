package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var (
	githubApiUrl     = "https://api.github.com"
	unleashRepoOwner = "nais"
	unleashRepoName  = "unleash"
)

func getLatestTags(owner, repo string) ([]string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/tags", githubApiUrl, owner, repo)

	resp, err := http.Get(url)
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
	err = json.NewDecoder(resp.Body).Decode(&tags)
	if err != nil {
		return nil, err
	}

	var tagNames []string
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

func UnleashVersions() ([]UnleashVersion, error) {
	tags, err := getLatestTags(unleashRepoOwner, unleashRepoName)
	if err != nil {
		return nil, err
	}

	versions := []UnleashVersion{}

	for _, tag := range tags {
		version, err := tagToUnleashVersion(tag)
		if err == nil {
			versions = append(versions, version)
		}
	}

	return versions, nil
}
