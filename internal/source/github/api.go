package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"
	"unicode"

	"github.com/enls/gremloader/internal/checksum"
	"github.com/enls/gremloader/internal/download"
)

const (
	apiPerPage  = 30
	maxAPIBytes = 16 << 20
	maxNameLen  = 255
)

type apiRelease struct {
	TagName     string     `json:"tag_name"`
	Name        string     `json:"name"`
	Draft       bool       `json:"draft"`
	Prerelease  bool       `json:"prerelease"`
	PublishedAt *time.Time `json:"published_at"`
	Assets      []struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		State  string `json:"state"`
		Size   int64  `json:"size"`
		Digest string `json:"digest"`
	} `json:"assets"`
}

// listAPI reads the most recent releases (one page, newest first by creation time, as GitHub
// orders them). A repository with more than one page of releases newer than the one wanted is
// not a Phase 1 case.
func (s *Source) listAPI(ctx context.Context, q Query) (*Listing, error) {
	resp, err := s.c.Get(ctx, download.Request{
		URL: fmt.Sprintf("%s/repos/%s/releases?per_page=%d", s.api, q.Repo, apiPerPage),
		Header: http.Header{
			"Accept":               {"application/vnd.github+json"},
			"X-GitHub-Api-Version": {"2022-11-28"},
		},
		MaxBytes: maxAPIBytes,
	})
	if err != nil {
		var se *download.StatusError
		if errors.As(err, &se) && se.Code == http.StatusNotFound {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, q.Repo)
		}
		return nil, err
	}

	var raw []apiRelease
	if err := json.Unmarshal(resp.Body, &raw); err != nil {
		return nil, fmt.Errorf("GitHub API answer for %s is not a release list: %w", q.Repo, err)
	}

	b := newBuilder(q)
	l := &Listing{Via: ModeAPI}
	for _, r := range raw {
		if r.Draft {
			continue
		}
		rel := Release{
			Tag:         r.TagName,
			Name:        r.Name,
			Prerelease:  r.Prerelease,
			FlagKnown:   true,
			AssetsKnown: true,
		}
		if r.PublishedAt != nil {
			rel.Published = *r.PublishedAt
		}
		// The tag is not validated until b.add below; until then it is shown quoted and shortened.
		shown := fmt.Sprintf("%q", truncate(r.TagName, 40))
		for _, a := range r.Assets {
			if !validAssetName(a.Name) {
				b.warnf("ignoring asset with unsafe name %q in %s", truncate(a.Name, 40), shown)
				continue
			}
			if a.State != "uploaded" {
				b.warnf("ignoring asset %s of %s: state is %q, not uploaded", a.Name, shown, a.State)
				continue
			}
			asset := Asset{Name: a.Name, ID: a.ID, Size: a.Size}
			// The digest is opportunistic: an older release has none, and a malformed one is
			// treated as absent (the checksum file is the baseline), with a note.
			if a.Digest != "" {
				if h, err := checksum.ParseDigest(a.Digest); err == nil {
					asset.SHA256 = h
				} else {
					b.warnf("ignoring digest of %s in %s: %v", a.Name, shown, err)
				}
			}
			asset.URL = s.assetURL(q.Repo, r.TagName, a.Name)
			rel.Assets = append(rel.Assets, asset)
		}
		if i := b.add(rel); i >= 0 && l.Latest == "" && !r.Prerelease {
			l.Latest = r.TagName
		}
	}
	l.Releases, l.Warnings = b.out, b.warn
	if len(l.Releases) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, noUsableTag(q))
	}
	return l, nil
}

// validAssetName reports whether name is safe to put in a URL path and to show.
func validAssetName(name string) bool {
	if name == "" || len(name) > maxNameLen || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\`) {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// SelectAsset picks the one asset of r that pattern names. pattern is a path.Match pattern, so a
// plain name matches exactly; zero matches or more than one is an error that lists the candidates
// (the PdfCraft repository publishes two similar Linux tarballs, and guessing would be wrong
// half the time).
//
// On the web path the asset list is unknown: a plain name is trusted and will fail at download
// time if wrong; a wildcard returns ErrNeedsAPI.
func (s *Source) SelectAsset(r Release, pattern string) (Asset, error) {
	if pattern == "" || strings.ContainsAny(pattern, `/\`) {
		return Asset{}, fmt.Errorf("asset name %q: must be a file name pattern", pattern)
	}
	if !r.AssetsKnown {
		if strings.ContainsAny(pattern, `*?[`) {
			return Asset{}, fmt.Errorf("%w: asset pattern %q needs the release's asset list", ErrNeedsAPI, pattern)
		}
		if !validAssetName(pattern) {
			return Asset{}, fmt.Errorf("asset name %q is not valid", pattern)
		}
		return Asset{Name: pattern, Size: -1, URL: s.assetURL(r.Repo, r.Tag, pattern)}, nil
	}

	// A name is its own match before it is a pattern: "tool-[x64].zip" is a file name here, and
	// read as a glob it would select "tool-x.zip" instead.
	for _, a := range r.Assets {
		if a.Name == pattern {
			return a, nil
		}
	}

	var matches []Asset
	for _, a := range r.Assets {
		ok, err := path.Match(pattern, a.Name)
		if err != nil {
			return Asset{}, fmt.Errorf("asset pattern %q: %w", pattern, err)
		}
		if ok {
			matches = append(matches, a)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return Asset{}, fmt.Errorf("release %s has no asset matching %q (it has: %s)", r.Tag, pattern, names(r.Assets))
	default:
		return Asset{}, fmt.Errorf("release %s has %d assets matching %q, want exactly one: %s",
			r.Tag, len(matches), pattern, names(matches))
	}
}

func names(assets []Asset) string {
	const limit = 10
	var out []string
	for i, a := range assets {
		if i == limit {
			out = append(out, fmt.Sprintf("... and %d more", len(assets)-limit))
			break
		}
		out = append(out, a.Name)
	}
	if len(out) == 0 {
		return "no finished assets"
	}
	return strings.Join(out, ", ")
}
