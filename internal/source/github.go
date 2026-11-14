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
	"strconv"
	"strings"
	"time"

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
		return networkError(err, fmt.Sprintf("cannot reach GitHub for %s", id))
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return errNotFound
	case resp.StatusCode != http.StatusOK:
		return statusError(resp, id, g.Token != "")
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return errs.Wrap(errs.Runtime, err, fmt.Sprintf("cannot read GitHub's answer for %s", id), "")
	}
	return nil
}

// networkError is a failure to talk to GitHub at all. Trying again may work.
func networkError(err error, message string) *errs.Error {
	e := errs.Wrap(errs.Runtime, err, message, "")
	e.Retryable = true
	return e
}

// statusError classifies an unexpected GitHub status: a rate limit (exit 1,
// retryable once the limit resets), a rejected or insufficient token (exit 3),
// or any other upstream failure (exit 1, retryable).
func statusError(resp *http.Response, id string, hadToken bool) *errs.Error {
	h := resp.Header
	remaining := h.Get("X-RateLimit-Remaining")
	limited := resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode == http.StatusForbidden && (remaining == "0" || h.Get("Retry-After") != ""))
	switch {
	case limited:
		msg := fmt.Sprintf("GitHub rate limit reached for %s", id)
		if remaining != "" {
			msg += fmt.Sprintf(": %s requests left", remaining)
		}
		if reset, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			msg += ", resets at " + time.Unix(reset, 0).UTC().Format(time.RFC3339)
		} else if after := h.Get("Retry-After"); after != "" {
			msg += ", retry after " + after + "s"
		}
		msg += "; set GITHUB_TOKEN to raise the limit"
		return &errs.Error{Kind: errs.Runtime, Code: "rate_limited", Message: msg, Retryable: true}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		msg := fmt.Sprintf("GitHub refused access to %s", id)
		if hadToken {
			msg += ": GITHUB_TOKEN was rejected or lacks access; set a valid token"
		} else {
			msg += ": the repo needs authentication; set GITHUB_TOKEN"
		}
		return errs.New(errs.Auth, msg, "")
	}
	return &errs.Error{
		Kind: errs.Runtime, Message: fmt.Sprintf("GitHub returned %s for %s", resp.Status, id), Retryable: true,
	}
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
	// flavors are the flavors release.json lists for any zip.
	var flavors []string
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
				flavors = append(flavors, m.Flavor)
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

	// The release isn't built for this flavor. Don't point at asset: a glob
	// skips the flavor check and would install another flavor's build.
	if len(candidates) == 0 && len(flavors) > 0 {
		slices.Sort(flavors)
		return Release{}, errs.New(errs.Conflict,
			fmt.Sprintf("%s doesn't support flavor %q (supports: %s)", what, spec.Flavor, strings.Join(slices.Compact(flavors), ", ")), "")
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
		return networkError(err, "download failed")
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return errs.New(errs.NotFound, "download not found: "+url, "")
	default:
		return statusError(resp, "download "+url, false)
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		return networkError(err, "download failed")
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
