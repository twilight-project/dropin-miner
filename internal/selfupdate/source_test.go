package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// fakeGitHub is a transport that answers like GitHub and records every
// request, with the production redirect policy in front of it.
func fakeGitHub(t *testing.T, answer func(*http.Request) *http.Response) (*HTTPSource, *[]*http.Request) {
	t.Helper()
	var seen []*http.Request
	client := NewHTTPClient()
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = append(seen, r)
		return answer(r), nil
	})
	return NewHTTPSource(client), &seen
}

func releaseJSON(tag string, extra string, assets ...string) string {
	var list []string
	for _, a := range assets {
		list = append(list, fmt.Sprintf(`{"name":%q,"size":10}`, a))
	}
	return fmt.Sprintf(`{"tag_name":%q%s,"assets":[%s]}`, tag, extra, strings.Join(list, ","))
}

func TestReleaseSelectsLatestOrTheExactTagFromTheFixedOrigin(t *testing.T) {
	src, seen := fakeGitHub(t, func(r *http.Request) *http.Response {
		if strings.HasSuffix(r.URL.Path, "/latest") {
			return response(http.StatusOK, releaseJSON("v0.3.1", "", "checksums.txt"))
		}
		return response(http.StatusOK, releaseJSON("v0.3.0", "", "checksums.txt"))
	})
	latest, err := src.Release(context.Background(), nil)
	if err != nil || latest.Version.String() != "0.3.1" {
		t.Fatalf("latest: %v %v", latest, err)
	}
	want, _ := ParseVersion("0.3.0")
	exact, err := src.Release(context.Background(), &want)
	if err != nil || exact.Version != want {
		t.Fatalf("exact: %v %v", exact, err)
	}
	if got := []string{(*seen)[0].URL.String(), (*seen)[1].URL.String()}; got[0] != githubAPIBase+"/latest" || got[1] != githubAPIBase+"/tags/v0.3.0" {
		t.Errorf("requests went to %v", got)
	}
	for _, r := range *seen {
		if r.URL.Scheme != "https" || r.URL.Host != "api.github.com" {
			t.Errorf("a release request left the fixed HTTPS origin: %s", r.URL)
		}
		for _, h := range []string{"Authorization", "Cookie", "Proxy-Authorization"} {
			if r.Header.Get(h) != "" {
				t.Errorf("a release request carried %s", h)
			}
		}
	}
	if NewHTTPSource(nil).apiBase != "https://api.github.com/repos/twilight-project/dropin-miner/releases" ||
		NewHTTPSource(nil).downloadBase != "https://github.com/twilight-project/dropin-miner/releases/download" {
		t.Error("the release origin is compiled in and must be the canonical repository")
	}
}

func TestReleaseRejectsWhatIsNotAStableCanonicalRelease(t *testing.T) {
	want, _ := ParseVersion("0.3.0")
	for name, body := range map[string]string{
		"malformed json":         `not json`,
		"prerelease flag":        releaseJSON("v0.3.0", `,"prerelease":true`),
		"draft":                  releaseJSON("v0.3.0", `,"draft":true`),
		"prerelease tag":         releaseJSON("v0.3.0-rc.1", ""),
		"tag without v":          releaseJSON("0.3.0", ""),
		"another tag than asked": releaseJSON("v0.3.1", ""),
		"duplicate asset":        releaseJSON("v0.3.0", "", "a", "a"),
		"unsafe asset name":      releaseJSON("v0.3.0", "", "../a"),
	} {
		src, _ := fakeGitHub(t, func(*http.Request) *http.Response { return response(http.StatusOK, body) })
		_, err := src.Release(context.Background(), &want)
		if err == nil || KindOf(err) != KindReleaseInvalid {
			t.Errorf("%s: want release_invalid, got %v", name, err)
		}
	}
}

func TestReleaseResponseBodyBound(t *testing.T) {
	src, _ := fakeGitHub(t, func(*http.Request) *http.Response {
		return response(http.StatusOK, strings.Repeat(" ", int(MaxReleaseJSONBytes))+releaseJSON("v0.3.0", ""))
	})
	if _, err := src.Release(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("an oversized release response must be refused, got %v", err)
	}
}

func TestReleaseStatusClassification(t *testing.T) {
	for status, kind := range map[int]Kind{
		http.StatusInternalServerError: KindUnavailable,
		http.StatusServiceUnavailable:  KindUnavailable,
		http.StatusTooManyRequests:     KindUnavailable,
		http.StatusForbidden:           KindUnavailable,
		http.StatusNotFound:            KindReleaseInvalid,
	} {
		src, _ := fakeGitHub(t, func(*http.Request) *http.Response { return response(status, "") })
		if _, err := src.Release(context.Background(), nil); KindOf(err) != kind {
			t.Errorf("HTTP %d: kind %q, want %q (%v)", status, KindOf(err), kind, err)
		}
	}
	client := NewHTTPClient()
	client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("dial refused") })
	if _, err := NewHTTPSource(client).Release(context.Background(), nil); KindOf(err) != KindUnavailable {
		t.Errorf("a network failure is a safe retry, got %v", err)
	}
}

func TestDownloadAssetsBounds(t *testing.T) {
	tag, _ := ParseVersion("0.3.0")
	body := "12345678901"
	src, seen := fakeGitHub(t, func(*http.Request) *http.Response { return response(http.StatusOK, body) })
	release := ReleaseInfo{Version: tag, Assets: map[string]ReleaseAsset{"a": {Name: "a", Size: 11}}}
	if _, err := src.DownloadAssets(context.Background(), release, []AssetRequirement{{Name: "a", MaxBytes: 10}}); err == nil {
		t.Error("an asset advertised over its bound was fetched")
	}
	if len(*seen) != 0 {
		t.Error("an asset advertised over its bound must not even be requested")
	}
	release.Assets["a"] = ReleaseAsset{Name: "a", Size: 10}
	if _, err := src.DownloadAssets(context.Background(), release, []AssetRequirement{{Name: "a", MaxBytes: 10}}); err == nil {
		t.Error("a body larger than its bound was accepted though the metadata was within it")
	}
	release.Assets[ChecksumAssetName] = ReleaseAsset{Name: ChecksumAssetName, Size: 1}
	body = strings.Repeat("x", int(MaxChecksumBytes)+1)
	if _, err := src.DownloadAssets(context.Background(), release, []AssetRequirement{{Name: ChecksumAssetName, MaxBytes: MaxChecksumBytes}}); err == nil {
		t.Error("checksums.txt over its bound was accepted")
	}
	body = "0123456789"
	got, err := src.DownloadAssets(context.Background(), release, []AssetRequirement{{Name: "a", MaxBytes: 10}})
	if err != nil || string(got["a"]) != body {
		t.Fatalf("an asset at its bound: %q %v", got["a"], err)
	}
	if last := (*seen)[len(*seen)-1].URL.String(); last != githubDownloadBase+"/v0.3.0/a" {
		t.Errorf("the download went to %s", last)
	}
}

func TestDownloadRequiresAnAdvertisedAssetAndAValidRequirement(t *testing.T) {
	tag, _ := ParseVersion("0.3.0")
	src, seen := fakeGitHub(t, func(*http.Request) *http.Response { return response(http.StatusOK, "x") })
	release := ReleaseInfo{Version: tag, Assets: map[string]ReleaseAsset{"a": {Name: "a", Size: 1}}}
	for name, reqs := range map[string][]AssetRequirement{
		"unadvertised":   {{Name: "missing", MaxBytes: 1}},
		"unsafe name":    {{Name: "../a", MaxBytes: 1}},
		"no bound":       {{Name: "a", MaxBytes: 0}},
		"required twice": {{Name: "a", MaxBytes: 1}, {Name: "a", MaxBytes: 1}},
	} {
		if _, err := src.DownloadAssets(context.Background(), release, reqs); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(*seen) > 1 {
		t.Errorf("invalid requirements must not reach the network (%d requests)", len(*seen))
	}
}

func TestRedirectPolicyIsHTTPSHostBoundedAndHopBounded(t *testing.T) {
	via := []*http.Request{{URL: mustURL(t, "https://github.com/start")}}
	for _, raw := range []string{
		"http://github.com/insecure",
		"https://github.com.evil.example/asset",
		"https://evil.example/github.com",
		"https://example.com/asset",
		"https://githubusercontent.com/asset",
	} {
		if err := checkReleaseRedirect(&http.Request{URL: mustURL(t, raw)}, via); err == nil {
			t.Errorf("redirect to %s was accepted", raw)
		}
	}
	for _, host := range []string{"github.com", "api.github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com"} {
		if err := checkReleaseRedirect(&http.Request{URL: mustURL(t, "https://"+host+"/asset")}, via); err != nil {
			t.Errorf("redirect to %s refused: %v", host, err)
		}
	}
	five := make([]*http.Request, maxRedirects)
	if err := checkReleaseRedirect(&http.Request{URL: mustURL(t, "https://github.com/x")}, five); err == nil {
		t.Error("a sixth redirect was accepted")
	}
	if err := checkReleaseRedirect(&http.Request{URL: mustURL(t, "https://github.com/x")}, five[:4]); err != nil {
		t.Errorf("a fifth redirect must be followed: %v", err)
	}
	if NewHTTPClient().CheckRedirect == nil || NewHTTPClient().Timeout != RequestTimeout {
		t.Error("the updater's client carries the redirect policy and the per-request timeout")
	}
}

func TestARefusedRedirectIsReleaseInvalidAndSaysReinstall(t *testing.T) {
	src, seen := fakeGitHub(t, func(r *http.Request) *http.Response {
		resp := response(http.StatusFound, "")
		resp.Header.Set("Location", "https://evil.example/dropin-miner.tar.gz")
		return resp
	})
	_, err := src.Release(context.Background(), nil)
	if KindOf(err) != KindReleaseInvalid || !strings.Contains(err.Error(), "reinstall with the installer") {
		t.Errorf("a refused redirect: %v", err)
	}
	if len(*seen) != 1 {
		t.Errorf("the refused host must never be requested (%d requests)", len(*seen))
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
