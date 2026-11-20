// Command relnotes builds release.json and the release notes for a tag from
// the CHANGELOG lines each merged pull request added. The release workflow
// runs it; it needs the full git history and GITHUB_TOKEN.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/raven-clown/transponder/internal/update"
)

func main() {
	version := flag.String("version", "", "the tag being released, such as v0.2.1")
	previous := flag.String("previous", "", "the tag before it; the highest lower tag if empty")
	repo := flag.String("repo", os.Getenv("GITHUB_REPOSITORY"), "owner/name")
	project := flag.String("project", "transponder", "project name")
	maintainers := flag.String("maintainers", "raven-clown", "comma-separated logins left out of New Contributors")
	out := flag.String("out", ".", "directory for release.json and notes.md")
	flag.Parse()
	if err := run(*version, *previous, *repo, *project, *maintainers, *out); err != nil {
		fmt.Fprintln(os.Stderr, "relnotes:", err)
		os.Exit(1)
	}
}

func run(version, previous, repo, project, maintainers, out string) error {
	if version == "" || repo == "" {
		return fmt.Errorf("-version and -repo are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if previous == "" {
		tags, err := git(ctx, "tag", "--list", "v*")
		if err != nil {
			return err
		}
		previous = update.PreviousTag(strings.Split(tags, "\n"), version)
	}
	date, err := git(ctx, "log", "-1", "--format=%cs", version)
	if err != nil {
		return err
	}
	src := &source{repo: repo, token: os.Getenv("GITHUB_TOKEN"), client: &http.Client{Timeout: 30 * time.Second}}
	m, err := update.Build(ctx, src, update.BuildOptions{
		Project: project, Repo: repo, Version: version, Previous: previous,
		Date: strings.TrimSpace(date), Maintainers: strings.Split(maintainers, ","),
	})
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, update.ManifestName), append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, "notes.md"), []byte(update.Notes(m)), 0o600)
}

func git(ctx context.Context, args ...string) (string, error) {
	b, err := exec.CommandContext(ctx, "git", args...).Output() //nolint:gosec // always git, args come from this tool
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(b), nil
}

type source struct {
	repo   string
	token  string
	client *http.Client
}

func (s *source) Commits(ctx context.Context, from, to string) ([]update.Commit, error) {
	rng := to
	if from != "" {
		rng = from + ".." + to
	}
	raw, err := git(ctx, "log", "--reverse", "--format=%H%x1f%s", rng)
	if err != nil {
		return nil, err
	}
	var commits []update.Commit
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if sha, subject, ok := strings.Cut(line, "\x1f"); ok {
			commits = append(commits, update.Commit{SHA: sha, Subject: subject})
		}
	}
	return commits, nil
}

func (s *source) ChangelogDiff(ctx context.Context, sha string) (string, error) {
	return git(ctx, "show", "--format=", "-U100000", sha, "--", "CHANGELOG.md")
}

func (s *source) PR(ctx context.Context, number int) (update.PR, error) {
	var raw struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		User  struct {
			Login string `json:"login"`
		} `json:"user"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := s.get(ctx, fmt.Sprintf("/repos/%s/pulls/%d", s.repo, number), &raw); err != nil {
		return update.PR{}, err
	}
	pr := update.PR{Number: number, Title: raw.Title, Author: raw.User.Login, Body: raw.Body}
	for _, l := range raw.Labels {
		pr.Labels = append(pr.Labels, l.Name)
	}
	return pr, nil
}

func (s *source) FirstMergedPR(ctx context.Context, author string) (int, error) {
	q := url.QueryEscape(fmt.Sprintf("repo:%s is:pr is:merged author:%s", s.repo, author))
	var raw struct {
		Items []struct {
			Number int `json:"number"`
		} `json:"items"`
	}
	if err := s.get(ctx, "/search/issues?sort=created&order=asc&per_page=1&q="+q, &raw); err != nil {
		return 0, err
	}
	if len(raw.Items) == 0 {
		return 0, nil
	}
	return raw.Items[0].Number, nil
}

func (s *source) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GET %s: %s %s", path, resp.Status, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}
