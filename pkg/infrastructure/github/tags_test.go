package github

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestUnleashVersions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == fmt.Sprintf("/repos/%s/%s/tags", unleashRepoOwner, unleashRepoName) {
			_, err := w.Write([]byte(`[
{
	"name": "v4.23.4-20230804-081623-e0123bf",
	"zipball_url": "https://api.github.com/repos/nais/unleash/zipball/refs/tags/v4.23.4-20230804-081623-e0123bf",
	"tarball_url": "https://api.github.com/repos/nais/unleash/tarball/refs/tags/v4.23.4-20230804-081623-e0123bf",
	"commit": {
		"sha": "e0123bf9800796183db2bc7069b614263f03070e",
		"url": "https://api.github.com/repos/nais/unleash/commits/e0123bf9800796183db2bc7069b614263f03070e"
	},
	"node_id": "MDM6UmVmMTE0MjQ0MDc0OnJlZnMvdGFncy92NC4yMy40LTIwMjMwODA0LTA4MTYyMy1lMDEyM2Jm"
},
{
	"name": "v4.23.4-20230804-074800-2dd1705",
	"zipball_url": "https://api.github.com/repos/nais/unleash/zipball/refs/tags/v4.23.4-20230804-074800-2dd1705",
	"tarball_url": "https://api.github.com/repos/nais/unleash/tarball/refs/tags/v4.23.4-20230804-074800-2dd1705",
	"commit": {
		"sha": "2dd1705d38a856c43b45ab67b1991db9480f7d07",
		"url": "https://api.github.com/repos/nais/unleash/commits/2dd1705d38a856c43b45ab67b1991db9480f7d07"
	},
	"node_id": "MDM6UmVmMTE0MjQ0MDc0OnJlZnMvdGFncy92NC4yMy40LTIwMjMwODA0LTA3NDgwMC0yZGQxNzA1"
},
{
	"name": "68.20191007.1233",
	"zipball_url": "https://api.github.com/repos/nais/unleash/zipball/refs/tags/68.20191007.1233",
	"tarball_url": "https://api.github.com/repos/nais/unleash/tarball/refs/tags/68.20191007.1233",
	"commit": {
		"sha": "016d0a6e3e6a4cda9eba9fedef8c7788dfb73e8e",
		"url": "https://api.github.com/repos/nais/unleash/commits/016d0a6e3e6a4cda9eba9fedef8c7788dfb73e8e"
	},
	"node_id": "MDM6UmVmMTE0MjQ0MDc0OnJlZnMvdGFncy82OC4yMDE5MTAwNy4xMjMz"
}
]`))
			assert.NoError(t, err)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	githubApiUrl = server.URL

	versions, err := UnleashVersions()
	assert.NoError(t, err)
	assert.NotEmpty(t, versions)

	for _, version := range versions {
		assert.NotEmpty(t, version.VersionNumber)
		assert.NotEqual(t, version.ReleaseTime, time.Time{})
		assert.NotEmpty(t, version.CommitHash)
	}
}

func TestTagToUnleashVersion(t *testing.T) {
	tests := []struct {
		tag         string
		expected    UnleashVersion
		expectedErr bool
		expectedMsg string
	}{
		{
			tag: "v1.0.0-20220101-010101-abcdefg",
			expected: UnleashVersion{
				VersionNumber: "1.0.0",
				ReleaseTime:   time.Date(2022, 0o1, 0o1, 0o1, 0o1, 0o1, 0, time.UTC),
				CommitHash:    "abcdefg",
				GitTag:        "v1.0.0-20220101-010101-abcdefg",
			},
			expectedErr: false,
			expectedMsg: "",
		},
		{
			tag:         "invalid-foo-bar",
			expected:    UnleashVersion{},
			expectedErr: true,
			expectedMsg: "invalid tag: invalid-foo-bar",
		},
		{
			tag:         "1.0.0-20220101-010101-abcdefg",
			expected:    UnleashVersion{},
			expectedErr: true,
			expectedMsg: "invalid tag: 1.0.0-20220101-010101-abcdefg",
		},
		{
			tag:         "v1.0.0-abcdefgh-abcdef-abcdefg",
			expected:    UnleashVersion{},
			expectedErr: true,
			expectedMsg: "invalid tag: v1.0.0-abcdefgh-abcdef-abcdefg",
		},
		{
			tag:         "v1.0.0-00000000-000000-abcdefg",
			expected:    UnleashVersion{},
			expectedErr: true,
			expectedMsg: "invalid release date/time: 00000000-000000",
		},
		{
			tag:         "v1.0.0-99999999-999999-abcdefg",
			expected:    UnleashVersion{},
			expectedErr: true,
			expectedMsg: "invalid release date/time: 99999999-999999",
		},
	}

	for _, test := range tests {
		version, err := tagToUnleashVersion(test.tag)

		if test.expectedErr {
			assert.Error(t, err)
			assert.Equal(t, test.expectedMsg, err.Error())
		} else {
			assert.NoError(t, err)
			assert.Equal(t, test.expected, version)
		}
	}
}

func TestGetLatestTags(t *testing.T) {
	testCases := []struct {
		name     string
		owner    string
		repo     string
		response string
		want     []string
		wantErr  bool
	}{
		{
			name:     "success",
			owner:    "test",
			repo:     "test",
			response: `[{"name": "v1.0.0"}, {"name": "v1.1.0"}, {"name": "v1.2.0"}]`,
			want:     []string{"v1.0.0", "v1.1.0", "v1.2.0"},
			wantErr:  false,
		},
		{
			name:     "http error",
			owner:    "test",
			repo:     "test",
			response: "",
			want:     nil,
			wantErr:  true,
		},
		{
			name:     "json error",
			owner:    "test",
			repo:     "test",
			response: `{"name": "v1.0.0"}`,
			want:     nil,
			wantErr:  true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == fmt.Sprintf("/repos/%s/%s/tags", tc.owner, tc.repo) {
					_, err := w.Write([]byte(tc.response))
					assert.NoError(t, err)
				} else {
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			githubApiUrl = server.URL

			got, err := getLatestTags(tc.owner, tc.repo)

			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			assert.Equal(t, tc.want, got)
		})
	}
}
