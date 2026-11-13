package source

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"path"
	"slices"
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
func (g *GitHub) Resolve(ctx context.Context, id string, spec Spec) (Release, error) {
	var r ghRelease
	var err error
	if spec.Version == Latest {
		r, err = g.resolveLatest(ctx, id)
	} else {
		r, err = g.resolveTag(ctx, id, spec.Version)
	}
	if err != nil {
		return Release{}, err
	}
	return g.pickZip(ctx, id, r, spec)
}

func (g *GitHub) resolveLatest(ctx context.Context, id string) (ghRelease, error) {
	url := fmt.Sprintf("%s/repos/%s/releases?per_page=100", strings.TrimRight(g.baseURL(), "/"), id)
	var releases []ghRelease
	if err := g.getJSON(ctx, url, id, &releases); errors.Is(err, errNotFound) {
		return ghRelease{}, errs.New(errs.NotFound, fmt.Sprintf("github:%s does not exist", id), "")
	} else if err != nil {
		return ghRelease{}, err
	}
	for _, r := range releases {
		if r.Draft || r.Prerelease {
			continue
		}
		return r, nil
	}
	return ghRelease{}, errs.New(errs.NotFound, fmt.Sprintf("github:%s has no published release", id), "")
}

func (g *GitHub) resolveTag(ctx context.Context, id, tag string) (ghRelease, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/tags/%s", strings.TrimRight(g.baseURL(), "/"), id, escapeTag(tag))
	var r ghRelease
	err := g.getJSON(ctx, url, id, &r)
	if err != nil && !errors.Is(err, errNotFound) {
		return ghRelease{}, err
	}
	if err != nil || r.Draft || r.Tag != tag {
		return ghRelease{}, errs.New(errs.NotFound, fmt.Sprintf("github:%s has no release tagged %q", id, tag), "")
	}
	return r, nil
}

// escapeTag escapes each segment of tag for a URL path but keeps its slashes,
// which GitHub's tag lookup expects.
func escapeTag(tag string) string {
	segs := strings.Split(tag, "/")
	for i, s := range segs {
		segs[i] = neturl.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// errNotFound is getJSON's answer for a 404.
var errNotFound = errors.New("not found")

// getJSON GETs url into out. A 404 returns errNotFound.
func (g *GitHub) getJSON(ctx context.Context, url, id string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	resp, err := g.client().Do(req)
	if err != nil {
		return errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot reach GitHub for %s", id), "")
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return errNotFound
	case resp.StatusCode != http.StatusOK:
		return &errs.Error{
			Kind: errs.Runtime, Message: fmt.Sprintf("GitHub returned %s for %s", resp.Status, id), Retryable: true,
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot read GitHub's answer for %s", id), "")
	}
	return nil
}

// releaseJSON is the part of the BigWigs packager's release.json that wam
// reads: one entry per zip, each listing the flavors it supports.
type releaseJSON struct {
	Releases []struct {
		Filename string `json:"filename"`
		Metadata []struct {
			Flavor string `json:"flavor"`
		} `json:"metadata"`
	} `json:"releases"`
}

// pickZip chooses one zip of r: the spec's asset glob, else the release.json
// entry for the spec's flavor, else the release's only zip.
func (g *GitHub) pickZip(ctx context.Context, id string, r ghRelease, spec Spec) (Release, error) {
	var zips []Release
	var manifestURL string
	for _, a := range r.Assets {
		switch {
		case strings.HasSuffix(strings.ToLower(a.Name), ".zip"):
			zips = append(zips, Release{Tag: r.Tag, Asset: a.Name, URL: a.URL})
		case a.Name == "release.json":
			manifestURL = a.URL
		}
	}
	slices.SortFunc(zips, func(a, b Release) int { return strings.Compare(a.Asset, b.Asset) })
	what := fmt.Sprintf("github:%s %s", id, r.Tag)
	if len(zips) == 0 {
		return Release{}, errs.New(errs.Conflict,
			fmt.Sprintf("%s has no installable zip, so it can't be installed from GitHub releases", what), "")
	}

	candidates := zips
	switch {
	case spec.Asset != "":
		candidates = filter(zips, func(z Release) bool { ok, _ := path.Match(spec.Asset, z.Asset); return ok })
	case manifestURL != "":
		var rj releaseJSON
		var buf bytes.Buffer
		if err := g.Download(ctx, manifestURL, &buf); err != nil {
			return Release{}, err
		}
		if err := json.Unmarshal(buf.Bytes(), &rj); err != nil {
			return Release{}, errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot read release.json of %s", what), "")
		}
		named := map[string]bool{}
		for _, e := range rj.Releases {
			for _, m := range e.Metadata {
				if m.Flavor == spec.Flavor {
					named[e.Filename] = true
				}
			}
		}
		candidates = filter(zips, func(z Release) bool { return named[z.Asset] })
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}

	// Zero or several matches: never guess. Show what the release has.
	shown := candidates
	reason := "matches several zips"
	if len(candidates) == 0 {
		shown, reason = zips, "has no zip that matches"
	}
	names := make([]string, len(shown))
	for i, z := range shown {
		names[i] = z.Asset
	}
	switch {
	case spec.Asset != "":
		reason = fmt.Sprintf("asset %q %s", spec.Asset, strings.TrimPrefix(reason, "has no zip that "))
	case manifestURL != "":
		reason = fmt.Sprintf("release.json %s for flavor %q", reason, spec.Flavor)
	default:
		reason = "has several zips"
	}
	return Release{}, errs.New(errs.Conflict,
		fmt.Sprintf("%s: %s (%s); set asset to the one you want", what, reason, strings.Join(names, ", ")), "")
}

func filter(zips []Release, keep func(Release) bool) []Release {
	var out []Release
	for _, z := range zips {
		if keep(z) {
			out = append(out, z)
		}
	}
	return out
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
