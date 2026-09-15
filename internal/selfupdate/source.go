package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/twilight-project/dropin-miner/internal/netdial"
)

// The one release origin, compiled in. No environment variable, flag or
// config redirects it; tests reach it only through the package's own seams.
const (
	releaseRepository  = "twilight-project/dropin-miner"
	githubAPIBase      = "https://api.github.com/repos/" + releaseRepository + "/releases"
	githubDownloadBase = "https://github.com/" + releaseRepository + "/releases/download"
	maxRedirects       = 5
)

// releaseHosts are the only hosts a request or redirect may reach: the API,
// the download page, and the two asset hosts GitHub has served downloads
// from. A deployed updater carries this list forever, so a host outside it
// is refused with advice to reinstall rather than followed.
var releaseHosts = map[string]bool{
	"api.github.com":                       true,
	"github.com":                           true,
	"objects.githubusercontent.com":        true,
	"release-assets.githubusercontent.com": true,
}

// ReleaseInfo is the closed subset of a GitHub release the updater reads.
type ReleaseInfo struct {
	Version Version
	Assets  map[string]ReleaseAsset
}

// ReleaseAsset is one asset GitHub advertises for a release.
type ReleaseAsset struct {
	Name string
	Size int64
}

// ReleaseSource selects a release and fetches named assets from it.
type ReleaseSource interface {
	Release(ctx context.Context, requested *Version) (ReleaseInfo, error)
	DownloadAssets(ctx context.Context, release ReleaseInfo, requirements []AssetRequirement) (map[string][]byte, error)
}

// HTTPSource is the canonical GitHub repository.
type HTTPSource struct {
	client       *http.Client
	apiBase      string
	downloadBase string
}

// NewHTTPSource talks to the canonical repository with client, or with
// NewHTTPClient's when client is nil.
func NewHTTPSource(client *http.Client) *HTTPSource {
	if client == nil {
		client = NewHTTPClient()
	}
	return &HTTPSource{client: client, apiBase: githubAPIBase, downloadBase: githubDownloadBase}
}

// selfupdateDialer is exactly the *net.Dialer http.DefaultTransport itself
// uses, named here rather than left inside a closure so a test can read
// its Timeout/KeepAlive directly.
var selfupdateDialer = &net.Dialer{Timeout: netdial.DefaultTimeout, KeepAlive: netdial.DefaultKeepAlive}

// selfupdateTransport is a clone of http.DefaultTransport, so the
// participant's proxy settings and every other DefaultTransport tuning
// (ForceAttemptHTTP2, TLSHandshakeTimeout, IdleConnTimeout, MaxIdleConns,
// ExpectContinueTimeout) still apply exactly as they did before this named
// the dial function explicitly — only DialContext is replaced, by
// netdial.For(selfupdateDialer). Package-level and constructed once: every
// client this package builds shares one connection pool.
var selfupdateTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = netdial.For(selfupdateDialer)
	return t
}()

// NewHTTPClient is the updater's only client: the shared dial seam (so the
// participant's proxy settings and TLS defaults apply, same as
// http.DefaultTransport), a per-request timeout, and a redirect policy that
// follows only HTTPS, at most five hops, to releaseHosts. It sends no
// credential.
func NewHTTPClient() *http.Client {
	return &http.Client{
		Timeout:       RequestTimeout,
		CheckRedirect: checkReleaseRedirect,
		Transport:     selfupdateTransport,
	}
}

var errRefusedRedirect = errors.New("the release download was redirected somewhere this updater does not trust; reinstall with the installer")

func checkReleaseRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("%w (more than %d redirects)", errRefusedRedirect, maxRedirects)
	}
	if req.URL.Scheme != "https" {
		return fmt.Errorf("%w (%s is not HTTPS)", errRefusedRedirect, req.URL.Redacted())
	}
	if !releaseHosts[strings.ToLower(req.URL.Hostname())] {
		return fmt.Errorf("%w (host %q)", errRefusedRedirect, req.URL.Hostname())
	}
	return nil
}

// Release returns GitHub's latest release when requested is nil, or exactly
// vX.Y.Z otherwise. Drafts, pre-releases and non-canonical tags are refused.
func (s *HTTPSource) Release(ctx context.Context, requested *Version) (ReleaseInfo, error) {
	endpoint := s.apiBase + "/latest"
	if requested != nil {
		endpoint = s.apiBase + "/tags/" + url.PathEscape(requested.Tag())
	}
	body, err := s.get(ctx, endpoint, "application/vnd.github+json", MaxReleaseJSONBytes)
	if err != nil {
		return ReleaseInfo{}, err
	}
	var payload struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ReleaseInfo{}, failure(KindReleaseInvalid, fmt.Errorf("GitHub release metadata is malformed: %w", err))
	}
	v, err := ParseReleaseTag(payload.TagName)
	if err != nil {
		return ReleaseInfo{}, failure(KindReleaseInvalid, fmt.Errorf("GitHub release: %w", err))
	}
	if requested != nil && v.Compare(*requested) != 0 {
		return ReleaseInfo{}, failure(KindReleaseInvalid, fmt.Errorf("GitHub returned %s for the requested %s", v.Tag(), requested.Tag()))
	}
	if payload.Draft || payload.Prerelease {
		return ReleaseInfo{}, failure(KindReleaseInvalid, fmt.Errorf("release %s is not a published stable release", v.Tag()))
	}
	assets := make(map[string]ReleaseAsset, len(payload.Assets))
	for _, a := range payload.Assets {
		if !safeAssetName(a.Name) || a.Size < 0 {
			return ReleaseInfo{}, failure(KindReleaseInvalid, fmt.Errorf("release %s has invalid metadata for asset %q", v.Tag(), a.Name))
		}
		if _, dup := assets[a.Name]; dup {
			return ReleaseInfo{}, failure(KindReleaseInvalid, fmt.Errorf("release %s lists asset %q more than once", v.Tag(), a.Name))
		}
		assets[a.Name] = ReleaseAsset{Name: a.Name, Size: a.Size}
	}
	return ReleaseInfo{Version: v, Assets: assets}, nil
}

// DownloadAssets fetches exactly the required assets, each advertised by the
// release and each under its own bound, both as advertised and as read.
func (s *HTTPSource) DownloadAssets(ctx context.Context, release ReleaseInfo, requirements []AssetRequirement) (map[string][]byte, error) {
	out := make(map[string][]byte, len(requirements))
	for _, req := range requirements {
		if !safeAssetName(req.Name) || req.MaxBytes <= 0 {
			return nil, failure(KindReleaseInvalid, fmt.Errorf("invalid verifier requirement for asset %q", req.Name))
		}
		if _, dup := out[req.Name]; dup {
			return nil, failure(KindReleaseInvalid, fmt.Errorf("verifier requires asset %q twice", req.Name))
		}
		meta, ok := release.Assets[req.Name]
		if !ok {
			return nil, failure(KindReleaseInvalid, fmt.Errorf("release %s has no asset %q", release.Version.Tag(), req.Name))
		}
		if meta.Size > req.MaxBytes {
			return nil, failure(KindReleaseInvalid, fmt.Errorf("asset %q is advertised at %d bytes, over its %d-byte limit", req.Name, meta.Size, req.MaxBytes))
		}
		endpoint := s.downloadBase + "/" + url.PathEscape(release.Version.Tag()) + "/" + url.PathEscape(req.Name)
		data, err := s.get(ctx, endpoint, "application/octet-stream", req.MaxBytes)
		if err != nil {
			return nil, err
		}
		out[req.Name] = data
	}
	return out, nil
}

// get performs one bounded GET and classifies what went wrong.
func (s *HTTPSource) get(ctx context.Context, endpoint, accept string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, failure(KindReleaseInvalid, err)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "dropin-miner-upgrade")
	resp, err := s.client.Do(req)
	if err != nil {
		if errors.Is(err, errRefusedRedirect) {
			return nil, failure(KindReleaseInvalid, err)
		}
		return nil, failure(KindUnavailable, fmt.Errorf("reach GitHub: %w", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		kind := KindReleaseInvalid
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden {
			kind = KindUnavailable // GitHub's rate limit answers 403 or 429
		}
		return nil, failure(kind, fmt.Errorf("GitHub answered %s for %s", resp.Status, endpoint))
	}
	data, err := readBounded(resp.Body, max)
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) {
			return nil, failure(KindUnavailable, fmt.Errorf("read %s: %w", endpoint, err))
		}
		return nil, failure(KindReleaseInvalid, fmt.Errorf("read %s: %w", endpoint, err))
	}
	return data, nil
}
