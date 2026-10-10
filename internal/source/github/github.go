// Package github lists a repository's releases and picks assets from them.
//
// Two paths lead to the same Listing, and the choice between them is the point of this package
// (ADR 0009):
//
//   - The web path asks github.com where /releases/latest points and reads releases.atom. It costs
//     no API quota, so a plain weekly update makes zero API requests. It cannot see GitHub's
//     prerelease flag, asset lists, asset state or digests.
//   - The API path reads /repos/{repo}/releases. It sees everything, and costs one unit of the
//     60-per-hour unauthenticated quota (5,000 with a token).
//
// With mode "auto" the web path is tried first unless the caller needs API-only data or a token is
// configured, and each path falls over to the other on failure. Nothing here decides *which*
// release to install; that is the resolver's job. Everything that came from the network and ends
// up in a URL or a file name is validated first, and download URLs are built here from validated
// parts, never copied from a response.
package github

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/enls/gremloader/internal/download"
	"github.com/enls/gremloader/internal/redact"
	"github.com/enls/gremloader/internal/ver"
)

// Default roots of the real service.
const (
	DefaultAPIBase = "https://api.github.com"
	DefaultWebBase = "https://github.com"
)

// Mode selects how releases are listed.
type Mode string

// The three modes of a package's `api` setting.
const (
	ModeAuto Mode = "auto"
	ModeAPI  Mode = "api"
	ModeWeb  Mode = "web"
)

// ParseMode reads a package file's `api` value; "" means auto.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case "", ModeAuto:
		return ModeAuto, nil
	case ModeAPI, ModeWeb:
		return Mode(s), nil
	}
	return "", fmt.Errorf("api = %q: want auto, api or web", s)
}

var (
	// ErrNotFound means the repository does not exist, has no releases, or is not visible.
	ErrNotFound = errors.New("repository or release not found")
	// ErrNeedsAPI means the request needs data only the GitHub API provides (prerelease flags,
	// asset lists, wildcard asset names) and the API is not usable.
	ErrNeedsAPI = errors.New("needs data only the GitHub API provides")
)

var repoName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}/[A-Za-z0-9._-]{1,100}$`)

// ValidRepo reports whether s is a plausible "owner/repo".
func ValidRepo(s string) bool {
	if !repoName.MatchString(s) {
		return false
	}
	for _, part := range strings.Split(s, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

// Options configures a Source.
type Options struct {
	APIBase string // default DefaultAPIBase
	WebBase string // default DefaultWebBase
}

// Source lists releases through a download.Client, which enforces every network rule.
type Source struct {
	c   *download.Client
	api string
	web string
}

// New returns a Source. The Client's token (if any) decides whether "auto" prefers the API.
func New(c *download.Client, o Options) *Source {
	api, web := o.APIBase, o.WebBase
	if api == "" {
		api = DefaultAPIBase
	}
	if web == "" {
		web = DefaultWebBase
	}
	return &Source{c: c, api: strings.TrimRight(api, "/"), web: strings.TrimRight(web, "/")}
}

// Query says what to list.
type Query struct {
	Repo      string // "owner/repo"
	TagPrefix string // stripped from tags before parsing versions ("" for none)
	Mode      Mode   // "" means auto
	// NeedAPIData is set when the answer is wrong without API-only data (the prerelease channel
	// needs GitHub's prerelease flag). The web path is then not an acceptable fallback.
	NeedAPIData bool
}

// Asset is one downloadable file of a release.
type Asset struct {
	Name   string
	ID     int64  // 0 when unknown (web path)
	Size   int64  // -1 when unknown (web path)
	SHA256 string // GitHub's digest as hex; "" when not reported (always, on the web path)
	URL    string // where to download it, built by this package
}

// Release is one release of the repository.
type Release struct {
	Repo    string
	Tag     string
	Version ver.Version
	Name    string
	// Prerelease is GitHub's flag, valid only when FlagKnown (API path). Without it, only a
	// semver pre-release identifier in Version marks a prerelease.
	Prerelease bool
	FlagKnown  bool
	// Published is zero when unknown.
	Published time.Time
	// Assets is the release's finished assets; valid only when AssetsKnown (API path).
	Assets      []Asset
	AssetsKnown bool
}

// Listing is the result of List.
type Listing struct {
	Releases []Release // drafts and unusable tags removed; source order, not sorted
	// Latest is the tag GitHub itself calls the latest stable release ("" if unknown or not
	// usable under the tag prefix).
	Latest string
	// Via is the path that produced the listing.
	Via Mode
	// FellOver says why the preferred path was not used ("" when it was).
	FellOver string
	// Warnings are things skipped, to be shown once.
	Warnings []string
}

// List lists the repository's releases.
func (s *Source) List(ctx context.Context, q Query) (*Listing, error) {
	if !ValidRepo(q.Repo) {
		return nil, fmt.Errorf("repository %q: want owner/repo", q.Repo)
	}
	mode := q.Mode
	if mode == "" {
		mode = ModeAuto
	}

	var order []Mode
	switch {
	case mode == ModeWeb && q.NeedAPIData:
		return nil, fmt.Errorf("%w: this package needs prerelease information but api = \"web\"", ErrNeedsAPI)
	case mode == ModeWeb:
		order = []Mode{ModeWeb}
	case mode == ModeAPI || q.NeedAPIData:
		order = []Mode{ModeAPI}
	case s.c.HasToken():
		order = []Mode{ModeAPI, ModeWeb}
	default:
		order = []Mode{ModeWeb, ModeAPI}
	}

	var errs []error
	for _, m := range order {
		var l *Listing
		var err error
		if m == ModeAPI {
			l, err = s.listAPI(ctx, q)
		} else {
			l, err = s.listWeb(ctx, q)
		}
		if err == nil {
			if len(errs) > 0 {
				l.FellOver = fmt.Sprintf("%s path failed: %s", order[0], redact.Text(errs[0].Error()))
			}
			return l, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		errs = append(errs, err)
		if !worthFallingOver(err) {
			break
		}
	}

	err := errors.Join(errs...)
	// "Needs the API" is the answer only when the API was refused for quota. A bad token or an
	// outage is a different failure and keeps its own error.
	var rl *download.RateLimitError
	if q.NeedAPIData && errors.As(err, &rl) {
		hint := "the API quota is used up"
		if !s.c.HasToken() {
			hint = "the prerelease channel needs the API; set GITHUB_TOKEN to raise the limit"
		}
		err = fmt.Errorf("%w (%s): %w", ErrNeedsAPI, hint, err)
	}
	return nil, err
}

// partialViewError is ErrNotFound as far as the website's feed goes. The feed shows only the most
// recent releases, so "no usable tag in it" says nothing about the repository: the API list is
// longer and may know the tag (a monorepo package whose releases have scrolled out of the feed).
// Unlike a plain ErrNotFound, the other path is worth trying.
type partialViewError struct{ msg string }

func (e *partialViewError) Error() string        { return ErrNotFound.Error() + ": " + e.msg }
func (e *partialViewError) Is(target error) bool { return target == ErrNotFound }

// worthFallingOver reports whether trying the other path could give a different answer.
func worthFallingOver(err error) bool {
	var pe *download.PolicyError
	var pv *partialViewError
	switch {
	case errors.As(err, &pv):
		return true
	case errors.Is(err, ErrNotFound), errors.Is(err, download.ErrTooLarge), errors.As(err, &pe):
		return false
	}
	return true
}

// listBuilder applies the tag rules shared by both paths.
type listBuilder struct {
	repo   string
	prefix string
	seen   map[string]bool
	out    []Release
	warn   []string
}

func newBuilder(q Query) *listBuilder {
	return &listBuilder{repo: q.Repo, prefix: q.TagPrefix, seen: map[string]bool{}}
}

func (b *listBuilder) warnf(format string, args ...any) {
	b.warn = append(b.warn, fmt.Sprintf(format, args...))
}

// add appends r if its tag is safe and parses as a version under the prefix. It returns the
// index of the release (existing or new), or -1 if the tag was rejected.
func (b *listBuilder) add(r Release) int {
	if b.seen[r.Tag] {
		for i := range b.out {
			if b.out[i].Tag == r.Tag {
				return i
			}
		}
		return -1 // rejected before, and already warned about
	}
	b.seen[r.Tag] = true
	if !ver.ValidTag(r.Tag) {
		b.warnf("ignoring release with unsafe tag %q", truncate(r.Tag, 40))
		return -1
	}
	if b.prefix != "" && !strings.HasPrefix(r.Tag, b.prefix) {
		return -1 // another package's tag in a shared repository: expected, not worth a warning
	}
	v, err := ver.FromTag(r.Tag, b.prefix)
	if err != nil {
		b.warnf("ignoring tag %s: %v", r.Tag, err)
		return -1
	}
	r.Repo, r.Version = b.repo, v
	b.out = append(b.out, r)
	return len(b.out) - 1
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// assetURL builds the public download URL of an asset from a validated tag and asset name.
func (s *Source) assetURL(repo, tag, name string) string {
	return fmt.Sprintf("%s/%s/releases/download/%s/%s", s.web, repo, url.PathEscape(tag), url.PathEscape(name))
}

// noUsableTag words "nothing matched", naming the tag prefix because a mistyped one is the usual
// reason.
func noUsableTag(q Query) string {
	msg := q.Repo + " has no release with a usable tag"
	if q.TagPrefix != "" {
		msg += fmt.Sprintf(" (tag prefix %q)", q.TagPrefix)
	}
	return msg
}
