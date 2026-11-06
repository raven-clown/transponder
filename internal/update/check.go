package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
)

// maxBody caps every response read, so a bad server can't make a check use
// much memory.
const maxBody = 4 << 20

// Checker asks GitHub which releases of a repository are newer than a version.
type Checker struct {
	// Repo is "owner/name".
	Repo string
	// APIBase is https://api.github.com unless a test points it elsewhere.
	APIBase string
	Client  *http.Client
}

// Report is the result of a check.
type Report struct {
	Current string
	Latest  string
	// Releases are the releases newer than Current, oldest first. For a
	// development build it holds only the latest release.
	Releases []Manifest
	// Dev is set when Current is not a release version.
	Dev bool
}

// Level is the most urgent level across the newer releases.
func (r *Report) Level() string {
	best := ""
	for _, m := range r.Releases {
		if best == "" || levelRank(m.Level) > levelRank(best) {
			best = m.Level
		}
	}
	return best
}

// UpToDate reports whether there is nothing newer to install.
func (r *Report) UpToDate() bool { return len(r.Releases) == 0 }

type ghRelease struct {
	TagName     string `json:"tag_name"`
	HTMLURL     string `json:"html_url"`
	Draft       bool   `json:"draft"`
	Prerelease  bool   `json:"prerelease"`
	PublishedAt string `json:"published_at"`
	Assets      []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// Check lists the releases newer than current and loads each one's manifest.
// A release without a manifest is still listed, with LevelUnknown.
func (c *Checker) Check(ctx context.Context, current string) (*Report, error) {
	base := c.APIBase
	if base == "" {
		base = "https://api.github.com"
	}
	var list []ghRelease
	if err := c.getJSON(ctx, base+"/repos/"+c.Repo+"/releases?per_page=50", &list); err != nil {
		return nil, fmt.Errorf("listing releases: %w", err)
	}
	type found struct {
		v   semver
		rel ghRelease
	}
	var all []found
	for _, rel := range list {
		if rel.Draft || rel.Prerelease {
			continue
		}
		if v, ok := parseVersion(rel.TagName); ok {
			all = append(all, found{v, rel})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].v.less(all[j].v) })
	rep := &Report{Current: current}
	if len(all) == 0 {
		return rep, nil
	}
	rep.Latest = all[len(all)-1].rel.TagName
	cur, ok := parseVersion(current)
	var newer []ghRelease
	if !ok {
		rep.Dev = true
		newer = []ghRelease{all[len(all)-1].rel}
	} else {
		for _, f := range all {
			if cur.less(f.v) {
				newer = append(newer, f.rel)
			}
		}
	}
	for _, rel := range newer {
		rep.Releases = append(rep.Releases, c.manifest(ctx, rel))
	}
	return rep, nil
}

func (c *Checker) manifest(ctx context.Context, rel ghRelease) Manifest {
	fallback := Manifest{Version: rel.TagName, URL: rel.HTMLURL, Level: LevelUnknown, Date: dateOf(rel.PublishedAt)}
	for _, a := range rel.Assets {
		if a.Name != ManifestName {
			continue
		}
		var m Manifest
		if err := c.getJSON(ctx, a.URL, &m); err != nil || m.Version != rel.TagName {
			return fallback
		}
		if m.URL == "" {
			m.URL = rel.HTMLURL
		}
		if m.Level == "" {
			m.Level = LevelOf(m.Changes)
		}
		return m
	}
	return fallback
}

func dateOf(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}

func (c *Checker) getJSON(ctx context.Context, target string, v any) error {
	client := c.Client
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return err
	}
	if len(body) > maxBody {
		return errors.New("response is too large")
	}
	return json.Unmarshal(body, v)
}
