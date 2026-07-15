package cache

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/synctv-org/vendors/api/emby"
	"github.com/zijiren233/go-uhc"
)

const testEmbyAPIKey = "test-householder-key"

func mustTestURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	return u
}

func textSubtitle(codec, mimeType, deliveryURL string) *emby.MediaStreamInfo {
	return &emby.MediaStreamInfo{
		Type:                 "Subtitle",
		IsTextSubtitleStream: true,
		Codec:                codec,
		MimeType:             mimeType,
		DeliveryUrl:          deliveryURL,
	}
}

func processOneTestSubtitle(t *testing.T, baseURL, deliveryURL string) []*EmbySubtitleCache {
	t.Helper()
	stream := textSubtitle("vtt", "", deliveryURL)
	return processEmbySubtitles(
		&emby.MediaSourceInfo{Id: "source", MediaStreamInfo: []*emby.MediaStreamInfo{stream}},
		"item", testEmbyAPIKey, mustTestURL(t, baseURL),
	)
}

func TestProcessMediaSourceDoesNotMutateSubtitleBase(t *testing.T) {
	base := mustTestURL(t, "https://emby.example/base/?binding=value")
	original := base.String()
	mediaSource := &emby.MediaSourceInfo{
		Id:        "source",
		Container: "mp4",
		MediaStreamInfo: []*emby.MediaStreamInfo{
			textSubtitle("vtt", "", "subs/a.vtt"),
		},
	}

	source, err := processMediaSource(
		mediaSource,
		nil,
		&EmbyUserCacheData{APIKey: testEmbyAPIKey},
		"item",
		base,
	)
	if err != nil || source == nil {
		t.Fatalf("process media source = (%#v, %v)", source, err)
	}
	if base.String() != original {
		t.Fatalf("base URL mutated: got %q, want %q", base, original)
	}

	subtitles := processEmbySubtitles(mediaSource, "item", testEmbyAPIKey, base)
	if len(subtitles) != 1 {
		t.Fatalf("subtitle count = %d, want 1", len(subtitles))
	}
	if got := mustTestURL(t, subtitles[0].URL).Path; got != "/base/subs/a.vtt" {
		t.Fatalf("subtitle path = %q, want base-relative path", got)
	}
}

func TestProcessEmbySubtitlesNormalizesSupportedTextFormats(t *testing.T) {
	tests := []struct {
		name        string
		codec       string
		mimeType    string
		deliveryURL string
		wantType    string
		wantContent string
	}{
		{name: "subrip codec", codec: "subrip", deliveryURL: "/subs/a.srt", wantType: "srt", wantContent: "application/x-subrip; charset=utf-8"},
		{name: "webvtt mime", mimeType: "text/vtt; charset=utf-8", deliveryURL: "/subs/a.vtt", wantType: "vtt", wantContent: "text/vtt; charset=utf-8"},
		{name: "all vtt evidence agrees", codec: "webvtt", mimeType: "text/vtt; charset=utf-8", deliveryURL: "/subs/a.vtt", wantType: "vtt", wantContent: "text/vtt; charset=utf-8"},
		{name: "ass codec", codec: "ass", deliveryURL: "/subs/a.ass", wantType: "ass", wantContent: "text/x-ssa; charset=utf-8"},
		{name: "ssa normalized to ass", codec: "ssa", deliveryURL: "/subs/a.ssa", wantType: "ass", wantContent: "text/x-ssa; charset=utf-8"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := mustTestURL(t, "https://emby.example/root/")
			original := base.String()
			stream := textSubtitle(tt.codec, tt.mimeType, tt.deliveryURL)
			got := processEmbySubtitles(
				&emby.MediaSourceInfo{Id: "source", MediaStreamInfo: []*emby.MediaStreamInfo{stream}},
				"item", testEmbyAPIKey, base,
			)
			if len(got) != 1 {
				t.Fatalf("subtitle count = %d, want 1", len(got))
			}
			if got[0].Type != tt.wantType || got[0].ContentType != tt.wantContent {
				t.Fatalf("format = (%q, %q), want (%q, %q)", got[0].Type, got[0].ContentType, tt.wantType, tt.wantContent)
			}
			if base.String() != original {
				t.Fatalf("base URL mutated: got %q, want %q", base, original)
			}
		})
	}
}

func TestProcessEmbySubtitlesUsesNegotiatedDeliveryFormat(t *testing.T) {
	tests := []struct {
		name        string
		codec       string
		mimeType    string
		deliveryURL string
	}{
		{name: "source subrip delivered as vtt", codec: "subrip", deliveryURL: "/subtitles/stream.vtt"},
		{name: "source srt delivered as vtt", codec: "srt", deliveryURL: "/subtitles/stream.vtt"},
		{name: "source ass delivered as vtt", codec: "ass", deliveryURL: "/subtitles/stream.vtt"},
		{name: "source ssa delivered as vtt", codec: "ssa", deliveryURL: "/subtitles/stream.vtt"},
		{name: "source ass with consistent vtt delivery evidence", codec: "ass", mimeType: "text/vtt", deliveryURL: "/subtitles/stream.vtt"},
		{name: "source vtt delivered as vtt", codec: "webvtt", deliveryURL: "/subtitles/stream.vtt"},
		{name: "mime proves extensionless vtt delivery", codec: "subrip", mimeType: "text/vtt", deliveryURL: "/subtitles/stream"},
		{name: "source subrip without delivery URL falls back to vtt", codec: "subrip"},
		{name: "vtt mime without delivery URL falls back to vtt", mimeType: "text/vtt"},
		{name: "ass mime without delivery URL falls back to vtt", mimeType: "text/x-ass"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stream := textSubtitle(tt.codec, tt.mimeType, tt.deliveryURL)
			stream.Index = 3
			got := processEmbySubtitles(
				&emby.MediaSourceInfo{Id: "synthetic-source", MediaStreamInfo: []*emby.MediaStreamInfo{stream}},
				"synthetic-item", testEmbyAPIKey, mustTestURL(t, "https://emby.example/root/"),
			)
			if len(got) != 1 {
				t.Fatalf("subtitle count = %d, want 1", len(got))
			}
			if got[0].Type != "vtt" || got[0].ContentType != "text/vtt; charset=utf-8" {
				t.Fatalf("format = (%q, %q), want (%q, %q)", got[0].Type, got[0].ContentType, "vtt", "text/vtt; charset=utf-8")
			}
		})
	}
}

func TestProcessEmbySubtitlesNilSourceIsSafe(t *testing.T) {
	if got := processEmbySubtitles(nil, "item", testEmbyAPIKey, mustTestURL(t, "https://emby.example/root/")); got != nil {
		t.Fatalf("subtitles = %#v, want nil", got)
	}
}

func TestProcessEmbySubtitlesOmitsUnsafeOrUnsupportedStreams(t *testing.T) {
	tests := []struct {
		name   string
		stream *emby.MediaStreamInfo
	}{
		{name: "nil stream", stream: nil},
		{name: "not subtitle", stream: &emby.MediaStreamInfo{Type: "Audio", IsTextSubtitleStream: true, Codec: "srt"}},
		{name: "subtitle type is case sensitive", stream: &emby.MediaStreamInfo{Type: "subtitle", IsTextSubtitleStream: true, Codec: "srt", DeliveryUrl: "/subs/a.srt"}},
		{name: "not text", stream: &emby.MediaStreamInfo{Type: "Subtitle", Codec: "srt"}},
		{name: "bitmap", stream: textSubtitle("pgs", "", "/subs/a.sup")},
		{name: "unknown codec with known vtt delivery", stream: textSubtitle("unknown", "", "/subs/a.vtt")},
		{name: "unknown mime", stream: textSubtitle("srt", "application/octet-stream", "/subs/a.srt")},
		{name: "unknown extension", stream: textSubtitle("srt", "", "/subs/a.bin")},
		{name: "delivery path has no extension", stream: textSubtitle("", "", "/subs/no-extension")},
		{name: "no format evidence", stream: textSubtitle("", "", "")},
		{name: "delivery mime extension conflict with known source", stream: textSubtitle("srt", "text/vtt", "/subs/a.srt")},
		{name: "mime extension conflict", stream: textSubtitle("", "text/vtt", "/subs/a.srt")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := processEmbySubtitles(
				&emby.MediaSourceInfo{MediaStreamInfo: []*emby.MediaStreamInfo{tt.stream}},
				"item", testEmbyAPIKey, mustTestURL(t, "https://emby.example/root/"),
			)
			if len(got) != 0 {
				t.Fatalf("subtitle count = %d, want 0", len(got))
			}
		})
	}
}

func TestProcessEmbySubtitlesAcceptsOnlySameOriginDeliveryURLs(t *testing.T) {
	accepted := []struct {
		name     string
		base     string
		delivery string
		wantPath string
	}{
		{name: "relative path", base: "https://emby.example/base/", delivery: "subs/a.vtt", wantPath: "/base/subs/a.vtt"},
		{name: "root relative path", base: "https://emby.example/base/", delivery: "/emby/subs/a.vtt", wantPath: "/emby/subs/a.vtt"},
		{name: "same origin absolute", base: "https://Emby.Example:443/base/", delivery: "HTTPS://emby.example/subs/a.vtt", wantPath: "/subs/a.vtt"},
		{name: "same explicit port", base: "http://emby.example:8096/base/", delivery: "http://EMBY.EXAMPLE:8096/subs/a.vtt", wantPath: "/subs/a.vtt"},
		{name: "same numeric port", base: "http://emby.example:80/base/", delivery: "http://emby.example:080/subs/a.vtt", wantPath: "/subs/a.vtt"},
	}
	for _, tt := range accepted {
		t.Run(tt.name, func(t *testing.T) {
			got := processOneTestSubtitle(t, tt.base, tt.delivery)
			if len(got) != 1 {
				t.Fatalf("subtitle count = %d, want 1", len(got))
			}
			parsed := mustTestURL(t, got[0].URL)
			if parsed.Path != tt.wantPath {
				t.Fatalf("path = %q, want %q", parsed.Path, tt.wantPath)
			}
		})
	}

	rejected := []struct {
		name     string
		base     string
		delivery string
	}{
		{name: "different scheme", base: "https://emby.example/", delivery: "http://emby.example/a.vtt"},
		{name: "different host", base: "https://emby.example/", delivery: "https://other.example/a.vtt"},
		{name: "different effective port", base: "https://emby.example/", delivery: "https://emby.example:444/a.vtt"},
		{name: "invalid port", base: "https://emby.example/", delivery: "https://emby.example:99999/a.vtt"},
		{name: "userinfo", base: "https://emby.example/", delivery: "https://user@emby.example/a.vtt"},
		{name: "fragment", base: "https://emby.example/", delivery: "/a.vtt#fragment"},
		{name: "non http", base: "https://emby.example/", delivery: "ftp://emby.example/a.vtt"},
		{name: "malformed", base: "https://emby.example/", delivery: "%"},
		{name: "malformed query escape", base: "https://emby.example/", delivery: "/a.vtt?safe=%ZZ"},
		{name: "unencoded semicolon", base: "https://emby.example/", delivery: "/a.vtt?safe=value;other=value"},
		{name: "mixed valid and malformed query", base: "https://emby.example/", delivery: "/a.vtt?safe=value&other=%ZZ"},
		{name: "cross origin scheme relative", base: "https://emby.example/", delivery: "//other.example/a.vtt"},
	}
	for _, tt := range rejected {
		t.Run(tt.name, func(t *testing.T) {
			if got := processOneTestSubtitle(t, tt.base, tt.delivery); len(got) != 0 {
				t.Fatalf("subtitle count = %d, want 0", len(got))
			}
		})
	}
}

func TestProcessEmbySubtitlesCanonicalizesAuthenticationQuery(t *testing.T) {
	got := processOneTestSubtitle(
		t,
		"https://emby.example/base/",
		"subs/a.vtt?safe=value&API_KEY=old&api_key=older&X-Emby-Token=token&x-emby-token=other",
	)
	if len(got) != 1 {
		t.Fatalf("subtitle count = %d, want 1", len(got))
	}
	query := mustTestURL(t, got[0].URL).Query()
	if query.Get("safe") != "value" {
		t.Fatalf("safe query = %q", query.Get("safe"))
	}
	if values := query["api_key"]; len(values) != 1 || values[0] != testEmbyAPIKey {
		t.Fatalf("canonical api_key = %#v", values)
	}
	for key := range query {
		if key != "api_key" && (strings.EqualFold(key, "api_key") || strings.EqualFold(key, "x-emby-token")) {
			t.Fatalf("non-canonical credential query remained: %q", key)
		}
	}
}

func TestProcessEmbySubtitlesFallbackRejectsInvalidBaseURLs(t *testing.T) {
	for _, rawBase := range []string{
		"https://user@emby.example/base/",
		"https://emby.example/base/#fragment",
		"https://emby.example:99999/base/",
		"ftp://emby.example/base/",
	} {
		t.Run(rawBase, func(t *testing.T) {
			stream := textSubtitle("vtt", "", "")
			got := processEmbySubtitles(
				&emby.MediaSourceInfo{Id: "source", MediaStreamInfo: []*emby.MediaStreamInfo{stream}},
				"item", testEmbyAPIKey, mustTestURL(t, rawBase),
			)
			if len(got) != 0 {
				t.Fatalf("subtitle count = %d, want 0", len(got))
			}
		})
	}
}

func TestProcessEmbySubtitlesFallbackIsAuthenticatedVTTAndDoesNotMutateBase(t *testing.T) {
	base := mustTestURL(t, "https://emby.example/base/path?original=value")
	original := base.String()
	streams := []*emby.MediaStreamInfo{
		{Type: "Subtitle", IsTextSubtitleStream: true, Codec: "srt", Index: 2, DisplayTitle: "English"},
		{Type: "Subtitle", IsTextSubtitleStream: true, Codec: "ass", Index: 7, DisplayTitle: "Japanese"},
	}
	got := processEmbySubtitles(
		&emby.MediaSourceInfo{Id: "source", MediaStreamInfo: streams},
		"item", testEmbyAPIKey, base,
	)
	if len(got) != 2 || got[0].URL == got[1].URL {
		t.Fatalf("fallback subtitles = %#v", got)
	}
	for i, subtitle := range got {
		parsed := mustTestURL(t, subtitle.URL)
		wantPath := []string{
			"/emby/Videos/item/source/Subtitles/2/Stream.vtt",
			"/emby/Videos/item/source/Subtitles/7/Stream.vtt",
		}[i]
		if parsed.Path != wantPath || parsed.Query().Get("api_key") != testEmbyAPIKey {
			t.Fatalf("fallback URL = %q, want path %q with key", subtitle.URL, wantPath)
		}
		if subtitle.Type != "vtt" || subtitle.ContentType != "text/vtt; charset=utf-8" {
			t.Fatalf("fallback format = (%q, %q)", subtitle.Type, subtitle.ContentType)
		}
	}
	if base.String() != original {
		t.Fatalf("base URL mutated: got %q, want %q", base, original)
	}
}

func TestNewEmbySubtitleCacheInitFuncFetchesBody(t *testing.T) {
	const body = "WEBVTT\n\n00:00.000 --> 00:01.000\nsubtitle\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api_key") != testEmbyAPIKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	got, err := newEmbySubtitleCacheInitFunc(server.URL + "/subtitle.vtt?api_key=" + testEmbyAPIKey)(context.Background())
	if err != nil {
		t.Fatalf("fetch subtitle: %v", err)
	}
	if string(got) != body {
		t.Fatalf("subtitle body = %q, want %q", got, body)
	}
}

func TestNewEmbySubtitleCacheInitFuncRejectsOversizedResponses(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "declared content length",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", strconv.FormatInt(subtitleMaxLength+1, 10))
				w.WriteHeader(http.StatusOK)
			},
		},
		{
			name: "unknown content length",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				flusher, _ := w.(http.Flusher)
				w.WriteHeader(http.StatusOK)
				if flusher != nil {
					flusher.Flush()
				}
				_, _ = io.CopyN(w, zeroReader{}, subtitleMaxLength+1)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			defer server.Close()

			_, err := newEmbySubtitleCacheInitFunc(server.URL + "/subtitle.vtt")(context.Background())
			if err == nil || err.Error() != "subtitle response too large" {
				t.Fatalf("error = %v, want fixed oversized error", err)
			}
			assertRedactedSubtitleError(t, err, []string{server.URL, "subtitle.vtt"})
		})
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestNewEmbySubtitleCacheInitFuncErrorsAreRedacted(t *testing.T) {
	const secret = "test-secret-that-must-not-leak"
	t.Run("network", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		rawURL := server.URL
		server.Close()
		_, err := newEmbySubtitleCacheInitFunc(rawURL + "/private/item/source?api_key=" + secret)(context.Background())
		assertRedactedSubtitleError(t, err, []string{rawURL, "private", "item", "source", "api_key", secret})
	})

	t.Run("status and body", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "upstream body "+secret, http.StatusBadGateway)
		}))
		defer server.Close()
		_, err := newEmbySubtitleCacheInitFunc(server.URL + "/private?api_key=" + secret)(context.Background())
		if err == nil || !strings.Contains(err.Error(), "502") {
			t.Fatalf("error = %v, want redacted status error", err)
		}
		assertRedactedSubtitleError(t, err, []string{server.URL, "private", "api_key", secret, "upstream body"})
	})
}

func TestNewEmbySubtitleHTTPClientPreservesSecuritySettings(t *testing.T) {
	client := newEmbySubtitleHTTPClient()
	if got := client.Transport; got != uhc.DefaultTransport {
		t.Fatalf("transport = %T, want uhc default transport", got)
	}
	if client.Timeout != embySubtitleHTTPTimeout || client.Timeout <= 0 {
		t.Fatalf("timeout = %s, want %s", client.Timeout, embySubtitleHTTPTimeout)
	}
}

func TestNewEmbySubtitleCacheInitFuncRejectsRedirectsWithoutCredentialForwarding(t *testing.T) {
	const secret = "redirect-test-key"
	var targetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests.Add(1)
		if r.URL.Query().Get("api_key") != "" {
			t.Errorf("redirect target received api_key")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	tests := []struct {
		name     string
		status   int
		location func(string) string
	}{
		{name: "301 same origin", status: http.StatusMovedPermanently, location: func(origin string) string { return origin + "/next.vtt" }},
		{name: "302 cross origin", status: http.StatusFound, location: func(string) string { return target.URL + "/next.vtt" }},
		{name: "303 same origin", status: http.StatusSeeOther, location: func(origin string) string { return origin + "/next.vtt" }},
		{name: "307 cross origin", status: http.StatusTemporaryRedirect, location: func(string) string { return target.URL + "/next.vtt" }},
		{name: "308 same origin", status: http.StatusPermanentRedirect, location: func(origin string) string { return origin + "/next.vtt" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var origin string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, tt.location(origin), tt.status)
			}))
			origin = server.URL
			defer server.Close()

			_, err := newEmbySubtitleCacheInitFunc(server.URL + "/start.vtt?api_key=" + secret)(context.Background())
			if err == nil || !strings.Contains(err.Error(), strconv.Itoa(tt.status)) {
				t.Fatalf("error = %v, want rejected redirect status %d", err, tt.status)
			}
			assertRedactedSubtitleError(t, err, []string{server.URL, target.URL, "api_key", secret})
		})
	}
	if got := targetRequests.Load(); got != 0 {
		t.Fatalf("cross-origin redirect target requests = %d, want 0", got)
	}
}

func assertRedactedSubtitleError(t *testing.T, err error, sensitive []string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected subtitle error")
	}
	for _, value := range sensitive {
		if strings.Contains(err.Error(), value) {
			t.Fatalf("error leaked %q: %q", value, err)
		}
	}
}
