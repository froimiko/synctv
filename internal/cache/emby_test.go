package cache

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func assertSingleEmbyVTTFallback(t *testing.T, got []*EmbySubtitleCache, index uint64) {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("subtitle count = %d, want 1", len(got))
	}
	if got[0] == nil {
		t.Fatal("subtitle = nil, want VTT fallback")
	}
	if got[0].Cache == nil {
		t.Fatal("subtitle cache = nil, want initialized cache")
	}

	parsed := mustTestURL(t, got[0].URL)
	wantPath := "/emby/Videos/item/source/Subtitles/" + strconv.FormatUint(index, 10) + "/Stream.vtt"
	if parsed.Path != wantPath {
		t.Fatalf("subtitle path = %q, want %q", parsed.Path, wantPath)
	}
	query := parsed.Query()
	if values := query["api_key"]; len(query) != 1 || len(values) != 1 || values[0] != testEmbyAPIKey {
		t.Fatalf("subtitle query = %#v, want only canonical api_key", query)
	}
	if got[0].Type != "vtt" || got[0].ContentType != "text/vtt; charset=utf-8" {
		t.Fatalf("subtitle format = (%q, %q), want VTT", got[0].Type, got[0].ContentType)
	}
}

func TestProcessMediaSourceDoesNotMutateBase(t *testing.T) {
	base := mustTestURL(t, "https://emby.example/base/?binding=value")
	original := base.String()
	mediaSource := &emby.MediaSourceInfo{
		Id:        "source",
		Container: "mp4",
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
}

func TestGetPlaybackInfoClassifiesUpstreamFailure(t *testing.T) {
	cause := errors.New("https://private.example/playback?api_key=secret")
	client := &playbackInfoResultClient{err: cause}

	data, err := getPlaybackInfo(context.Background(), client, &EmbyUserCacheData{
		Host:   "https://emby.example",
		APIKey: "secret",
		UserID: "emby-user",
	}, "item")
	if data != nil || !errors.Is(err, cause) {
		t.Fatalf("playback info = (%#v, %v)", data, err)
	}
	if category := EmbyDiagnosticErrorCategory(err); category != "playback_info_failed" {
		t.Fatalf("category = %q, want playback_info_failed", category)
	}
	assertRedactedSubtitleError(t, err, []string{"private.example", "api_key", "secret"})
}

func TestProcessPlaybackInfoResponseClassifiesEmptyAndInvalidSources(t *testing.T) {
	binding := &EmbyUserCacheData{Host: "https://emby.example", APIKey: testEmbyAPIKey}
	tests := []struct {
		name         string
		response     *emby.PlaybackInfoResp
		wantCategory string
		wantSources  int
		wantStreams  int
		wantSubs     int
	}{
		{
			name:         "nil response",
			response:     nil,
			wantCategory: "playback_info_empty",
			wantSources:  -1,
			wantStreams:  -1,
			wantSubs:     -1,
		},
		{
			name:         "empty media sources",
			response:     &emby.PlaybackInfoResp{},
			wantCategory: "playback_info_empty",
			wantSources:  0,
			wantStreams:  -1,
			wantSubs:     -1,
		},
		{
			name: "all nil or unplayable sources",
			response: &emby.PlaybackInfoResp{MediaSourceInfo: []*emby.MediaSourceInfo{
				nil,
				{
					MediaStreamInfo: []*emby.MediaStreamInfo{
						{Type: "Audio"},
						{Type: "Subtitle"},
					},
				},
			}},
			wantCategory: "media_source_processing_failed",
			wantSources:  2,
			wantStreams:  2,
			wantSubs:     1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := processPlaybackInfoResponse(tt.response, nil, binding, "item")
			if data != nil || err == nil {
				t.Fatalf("processed response = (%#v, %v), want error", data, err)
			}
			details, ok := EmbyDiagnosticDetailsFromError(err)
			if !ok {
				t.Fatalf("missing diagnostic details: %v", err)
			}
			if details.Category != tt.wantCategory || details.SourceCount != tt.wantSources ||
				details.MediaStreamCount != tt.wantStreams || details.SubtitleCount != tt.wantSubs {
				t.Fatalf("details = %#v", details)
			}
		})
	}
}

func TestProcessPlaybackInfoResponseKeepsStableIndexesWhenAnySourceIsValid(t *testing.T) {
	binding := &EmbyUserCacheData{Host: "https://emby.example", APIKey: testEmbyAPIKey}
	response := &emby.PlaybackInfoResp{
		PlaySessionID: "session",
		MediaSourceInfo: []*emby.MediaSourceInfo{
			nil,
			{},
			{Id: "source", Container: "mp4"},
		},
	}

	data, err := processPlaybackInfoResponse(response, nil, binding, "item")
	if err != nil {
		t.Fatalf("process playback info: %v", err)
	}
	if data == nil || data.TranscodeSessionID != "session" || len(data.Sources) != 3 {
		t.Fatalf("cache data = %#v", data)
	}
	if data.Sources[0].URL != "" || data.Sources[1].URL != "" || data.Sources[2].URL == "" {
		t.Fatalf("stable source indexes were not preserved: %#v", data.Sources)
	}
}

func TestProcessMediaSourceTreatsDirectURLAsValidWithoutContainer(t *testing.T) {
	source, err := processMediaSource(
		&emby.MediaSourceInfo{Id: "source", DirectPlayUrl: "/Videos/item/stream"},
		nil,
		&EmbyUserCacheData{Host: "https://emby.example", APIKey: testEmbyAPIKey},
		"item",
		mustTestURL(t, "https://emby.example"),
	)
	if err != nil || source == nil || source.URL == "" {
		t.Fatalf("direct source = (%#v, %v)", source, err)
	}
}

type playbackInfoResultClient struct {
	emby.UnimplementedEmbyServer
	response *emby.PlaybackInfoResp
	err      error
}

func (c *playbackInfoResultClient) PlaybackInfo(
	context.Context,
	*emby.PlaybackInfoReq,
) (*emby.PlaybackInfoResp, error) {
	return c.response, c.err
}

func TestProcessEmbySubtitlesListsEverySubtitleAsAuthenticatedVTTFallback(t *testing.T) {
	const streamIndex = uint64(7)
	tests := []struct {
		name   string
		stream *emby.MediaStreamInfo
	}{
		{
			name: "text flag is false",
			stream: &emby.MediaStreamInfo{
				Type: "Subtitle", Codec: "srt", DeliveryUrl: "/subtitles/original.srt",
			},
		},
		{
			name: "codec is empty",
			stream: &emby.MediaStreamInfo{
				Type: "Subtitle", IsTextSubtitleStream: true,
			},
		},
		{
			name: "codec is unknown",
			stream: &emby.MediaStreamInfo{
				Type: "Subtitle", IsTextSubtitleStream: true, Codec: "made-up-codec",
			},
		},
		{
			name: "mime is unknown",
			stream: &emby.MediaStreamInfo{
				Type: "Subtitle", IsTextSubtitleStream: true, Codec: "srt",
				MimeType: "application/octet-stream", DeliveryUrl: "/subtitles/original.srt",
			},
		},
		{
			name: "delivery URL is invalid",
			stream: &emby.MediaStreamInfo{
				Type: "Subtitle", IsTextSubtitleStream: true, Codec: "srt", DeliveryUrl: "%",
			},
		},
		{
			name: "delivery URL is cross origin",
			stream: &emby.MediaStreamInfo{
				Type: "Subtitle", IsTextSubtitleStream: true, Codec: "vtt",
				DeliveryUrl: "https://other.example/subtitles/original.vtt",
			},
		},
		{
			name: "same-origin delivery URL is ignored",
			stream: &emby.MediaStreamInfo{
				Type: "Subtitle", IsTextSubtitleStream: true, Codec: "vtt",
				DeliveryUrl: "/subtitles/original.vtt?api_key=stale&X-Emby-Token=stale",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.stream.Index = streamIndex
			base := mustTestURL(t, "https://emby.example/base/?existing=value&API_KEY=stale")
			original := base.String()
			got := processEmbySubtitles(
				&emby.MediaSourceInfo{Id: "source", MediaStreamInfo: []*emby.MediaStreamInfo{tt.stream}},
				"item", testEmbyAPIKey, base,
			)

			assertSingleEmbyVTTFallback(t, got, streamIndex)
			if base.String() != original {
				t.Fatalf("base URL mutated: got %q, want %q", base, original)
			}
		})
	}
}

func TestProcessEmbySubtitlesNilSourceIsSafe(t *testing.T) {
	if got := processEmbySubtitles(nil, "item", testEmbyAPIKey, mustTestURL(t, "https://emby.example/root/")); got != nil {
		t.Fatalf("subtitles = %#v, want nil", got)
	}
}

func TestProcessEmbySubtitlesIgnoresNilAndNonSubtitleStreams(t *testing.T) {
	tests := []struct {
		name   string
		stream *emby.MediaStreamInfo
	}{
		{name: "nil stream", stream: nil},
		{name: "audio stream", stream: &emby.MediaStreamInfo{Type: "Audio", IsTextSubtitleStream: true, Codec: "srt"}},
		{name: "subtitle type is case sensitive", stream: &emby.MediaStreamInfo{Type: "subtitle", IsTextSubtitleStream: true, Codec: "srt"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := processEmbySubtitles(
				&emby.MediaSourceInfo{Id: "source", MediaStreamInfo: []*emby.MediaStreamInfo{tt.stream}},
				"item", testEmbyAPIKey, mustTestURL(t, "https://emby.example/root/"),
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

func TestEmbyDiagnosticErrorClassificationIsStableAndRedacted(t *testing.T) {
	const secret = "diagnostic-secret"
	cause := errors.New("https://private.example/item?api_key=" + secret)
	err := NewEmbyDiagnosticError("subtitle_cache_fetch_failed", cause)

	if got := EmbyDiagnosticErrorCategory(err); got != "subtitle_cache_fetch_failed" {
		t.Fatalf("category = %q", got)
	}
	if !errors.Is(err, cause) {
		t.Fatal("diagnostic error did not preserve cause")
	}
	assertRedactedSubtitleError(t, err, []string{"private.example", "api_key", secret})
}

func TestNewEmbySubtitleCacheInitFuncClassifiesUpstreamStatus(t *testing.T) {
	const secret = "status-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "sensitive response "+secret, http.StatusBadGateway)
	}))
	defer server.Close()

	_, err := newEmbySubtitleCacheInitFunc(server.URL + "/private?api_key=" + secret)(context.Background())
	details, ok := EmbyDiagnosticDetailsFromError(err)
	if !ok || details.Category != "subtitle_upstream_status" || details.HTTPStatus != http.StatusBadGateway {
		t.Fatalf("diagnostic details = %#v, ok = %v", details, ok)
	}
	assertRedactedSubtitleError(t, err, []string{server.URL, "private", "api_key", secret, "sensitive response"})
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
			if category := EmbyDiagnosticErrorCategory(err); category != "subtitle_response_too_large" {
				t.Fatalf("category = %q, want subtitle_response_too_large", category)
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
	if embySubtitleHTTPTimeout != 30*time.Second {
		t.Fatalf("subtitle HTTP timeout = %s, want 30s", embySubtitleHTTPTimeout)
	}
	if subtitleMaxLength != 15*1024*1024 {
		t.Fatalf("subtitle max length = %d, want %d", subtitleMaxLength, 15*1024*1024)
	}

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
