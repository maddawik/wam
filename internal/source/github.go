package source

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strings"

	"github.com/maddawik/wam/internal/errs"
)

// DefaultGitHubAPI is the public GitHub REST API.
const DefaultGitHubAPI = "https://api.github.com"

// GitHub resolves addons from GitHub Releases.
type GitHub struct {
	// BaseURL is the API root. Tests point it at a fake server.
	BaseURL string
	// Token is sent with API requests when set.
	Token string
	// Client defaults to http.DefaultClient.
	Client *http.Client
}

type ghRelease struct {
	Tag        string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// Resolve finds the release of the repo id ("owner/repo") that version names.
// "latest" is the newest non-draft, non-prerelease release. Anything else is
// an exact tag, matched as written, and may be a prerelease.
func (g *GitHub) Resolve(ctx context.Context, id, version string) (Release, error) {
	if version == "latest" {
		return g.resolveLatest(ctx, id)
	}
	return g.resolveTag(ctx, id, version)
}

func (g *GitHub) resolveLatest(ctx context.Context, id string) (Release, error) {
	url := fmt.Sprintf("%s/repos/%s/releases?per_page=100", strings.TrimRight(g.baseURL(), "/"), id)
	var releases []ghRelease
	if found, err := g.getJSON(ctx, url, id, &releases); err != nil {
		return Release{}, err
	} else if !found {
		return Release{}, errs.New(errs.NotFound, fmt.Sprintf("github:%s does not exist", id), "")
	}
	for _, r := range releases {
		if r.Draft || r.Prerelease {
			continue
		}
		return pickZip(id, r)
	}
	return Release{}, errs.New(errs.NotFound, fmt.Sprintf("github:%s has no published release", id), "")
}

func (g *GitHub) resolveTag(ctx context.Context, id, tag string) (Release, error) {
	segs := strings.Split(tag, "/")
	for i, s := range segs {
		segs[i] = neturl.PathEscape(s)
	}
	url := fmt.Sprintf("%s/repos/%s/releases/tags/%s", strings.TrimRight(g.baseURL(), "/"), id, strings.Join(segs, "/"))
	var r ghRelease
	found, err := g.getJSON(ctx, url, id, &r)
	if err != nil {
		return Release{}, err
	}
	if !found || r.Draft || r.Tag != tag {
		return Release{}, errs.New(errs.NotFound, fmt.Sprintf("github:%s has no release tagged %q", id, tag), "")
	}
	return pickZip(id, r)
}

// getJSON GETs url into out. found is false on a 404.
func (g *GitHub) getJSON(ctx context.Context, url, id string, out any) (found bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	resp, err := g.client().Do(req)
	if err != nil {
		return false, errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot reach GitHub for %s", id), "")
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	case resp.StatusCode != http.StatusOK:
		return false, &errs.Error{
			Kind: errs.Runtime, Message: fmt.Sprintf("GitHub returned %s for %s", resp.Status, id), Retryable: true,
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return false, errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot read GitHub's answer for %s", id), "")
	}
	return true, nil
}

// pickZip requires r to have exactly one .zip asset.
func pickZip(id string, r ghRelease) (Release, error) {
	var zips []Release
	for _, a := range r.Assets {
		if strings.HasSuffix(strings.ToLower(a.Name), ".zip") {
			zips = append(zips, Release{Tag: r.Tag, Asset: a.Name, URL: a.URL})
		}
	}
	switch len(zips) {
	case 1:
		return zips[0], nil
	case 0:
		return Release{}, errs.New(errs.Conflict, fmt.Sprintf("github:%s %s has no zip asset", id, r.Tag), "")
	default:
		names := make([]string, len(zips))
		for i, z := range zips {
			names[i] = z.Asset
		}
		return Release{}, errs.New(errs.Conflict,
			fmt.Sprintf("github:%s %s has several zip assets (%s) and wam cannot choose yet", id, r.Tag, strings.Join(names, ", ")), "")
	}
}

// Download writes the asset at url to w.
func (g *GitHub) Download(ctx context.Context, url string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := g.client().Do(req)
	if err != nil {
		return errs.Wrap(errs.Runtime, err, "download failed", "")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return &errs.Error{Kind: errs.Runtime, Message: fmt.Sprintf("download returned %s", resp.Status), Retryable: true}
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		return errs.Wrap(errs.Runtime, err, "download failed", "")
	}
	return nil
}

func (g *GitHub) baseURL() string {
	if g.BaseURL == "" {
		return DefaultGitHubAPI
	}
	return g.BaseURL
}

func (g *GitHub) client() *http.Client {
	if g.Client == nil {
		return http.DefaultClient
	}
	return g.Client
}
