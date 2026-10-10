package github

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/enls/gremloader/internal/download"
	"github.com/enls/gremloader/internal/ver"
)

const (
	tagMarker       = "/releases/tag/"
	maxLatestHops   = 4        // /releases/latest of a renamed or moved repository redirects first
	maxFeedBytes    = 16 << 20 // releases.atom carries every release's notes, about 1 MB for ten
	maxRedirectBody = 1 << 20
)

// listWeb is the quota-free path: the /releases/latest redirect for "latest stable" and
// releases.atom for the recent tags with their publish times.
func (s *Source) listWeb(ctx context.Context, q Query) (*Listing, error) {
	latest, err := s.webLatest(ctx, q.Repo)
	if err != nil {
		return nil, err
	}

	b := newBuilder(q)
	l := &Listing{Via: ModeWeb}

	feed, ferr := s.c.Get(ctx, download.Request{
		URL:      fmt.Sprintf("%s/%s/releases.atom", s.web, q.Repo),
		Header:   http.Header{"Accept": {"application/atom+xml, application/xml;q=0.9, */*;q=0.1"}},
		MaxBytes: maxFeedBytes,
	})
	switch {
	case ferr == nil:
		entries, perr := parseAtom(feed.Body)
		if perr != nil {
			b.warnf("release feed unreadable (%v): publish times unknown", perr)
		}
		for _, e := range entries {
			b.add(Release{Tag: e.tag, Name: e.title, Published: e.updated})
		}
	case ctx.Err() != nil:
		return nil, ctx.Err()
	default:
		var pe *download.PolicyError
		if errors.As(ferr, &pe) {
			return nil, ferr
		}
		b.warnf("release feed unavailable (%v): publish times unknown", ferr)
	}

	// The redirect's answer must be a candidate even if the feed did not list it (the feed is
	// short, and may be missing).
	if latest != "" {
		if i := b.add(Release{Tag: latest}); i >= 0 {
			l.Latest = latest
		} else if q.TagPrefix != "" && ver.ValidTag(latest) && !strings.HasPrefix(latest, q.TagPrefix) {
			// Not an anomaly, but it is why Latest is empty, so it is said once.
			b.warnf("GitHub's latest release is %s, which is not under the tag prefix %q", latest, q.TagPrefix)
		}
	}

	l.Releases, l.Warnings = b.out, b.warn
	if len(l.Releases) == 0 {
		return nil, &partialViewError{msg: noUsableTag(q) + " among the feed's recent ones"}
	}
	return l, nil
}

// webLatest returns the tag github.com/{repo}/releases/latest redirects to. A repository that was
// renamed or transferred redirects to the new /releases/latest first, so follow a few hops by hand,
// checking each target against the policy.
func (s *Source) webLatest(ctx context.Context, repo string) (string, error) {
	next := fmt.Sprintf("%s/%s/releases/latest", s.web, repo)
	for range maxLatestHops {
		resp, err := s.c.Get(ctx, download.Request{URL: next, NoFollow: true, MaxBytes: maxRedirectBody})
		if err != nil {
			var se *download.StatusError
			if errors.As(err, &se) && se.Code == http.StatusNotFound {
				return "", fmt.Errorf("%w: %s", ErrNotFound, repo)
			}
			return "", err
		}
		if resp.Status < 300 || resp.Status >= 400 {
			return "", fmt.Errorf("%s/releases/latest answered HTTP %d, not a redirect", repo, resp.Status)
		}
		base, err := url.Parse(next)
		if err != nil {
			return "", err
		}
		target, err := base.Parse(resp.Header.Get("Location"))
		if err != nil || resp.Header.Get("Location") == "" {
			return "", fmt.Errorf("%s/releases/latest redirected without a usable Location", repo)
		}
		if err := s.c.Allowed(target.String()); err != nil {
			var pe *download.PolicyError
			if errors.As(err, &pe) {
				pe.Redirect = true
			}
			return "", err
		}
		if i := strings.LastIndex(target.Path, tagMarker); i >= 0 {
			return target.Path[i+len(tagMarker):], nil
		}
		if strings.HasSuffix(target.Path, "/releases/latest") {
			next = target.String()
			continue
		}
		return "", fmt.Errorf("%w: %s has no releases", ErrNotFound, repo)
	}
	return "", fmt.Errorf("%s/releases/latest redirected more than %d times", repo, maxLatestHops)
}

type atomEntry struct {
	tag     string
	title   string
	updated time.Time // zero when missing or unparsable
}

type atomFeed struct {
	Entries []struct {
		Updated string `xml:"updated"`
		Title   string `xml:"title"`
		Links   []struct {
			Rel  string `xml:"rel,attr"`
			Href string `xml:"href,attr"`
		} `xml:"link"`
	} `xml:"entry"`
}

// parseAtom reads the entries of a releases.atom feed. The tag comes from the entry's alternate
// link (.../releases/tag/<tag>); the entry's <updated> is the publish time.
func parseAtom(b []byte) ([]atomEntry, error) {
	var f atomFeed
	if err := xml.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	var out []atomEntry
	for _, e := range f.Entries {
		var tag string
		for _, l := range e.Links {
			if l.Rel != "alternate" {
				continue
			}
			u, err := url.Parse(l.Href)
			if err != nil {
				continue
			}
			if i := strings.LastIndex(u.Path, tagMarker); i >= 0 {
				tag = u.Path[i+len(tagMarker):]
				break
			}
		}
		if tag == "" {
			continue
		}
		t, _ := time.Parse(time.RFC3339, strings.TrimSpace(e.Updated))
		out = append(out, atomEntry{tag: tag, title: strings.TrimSpace(e.Title), updated: t})
	}
	return out, nil
}
