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

func assertCanonicalEmbyAPIKey(t *testing.T, query url.Values, want url.Values) {
	t.Helper()
	if query.Encode() != want.Encode() {
		t.Fatalf("subtitle query = %#v, want %#v", query, want)
	}
	for key, values := range query {
		if strings.EqualFold(key, "api_key") || strings.EqualFold(key, "x-emby-token") {
			if key != "api_key" || len(values) != 1 || values[0] != testEmbyAPIKey {
				t.Fatalf("subtitle credentials = %q: %#v, want one canonical api_key", key, values)
			}
		}
	}
}

func assertSingleEmbyFallback(
	t *testing.T,
	got []*EmbySubtitleCache,
	itemID string,
	sourceID string,
	index uint64,
	subtitleType string,
	contentType string,
) {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("subtitle count = %d, want 1", len(got))
	}
	if got[0] == nil {
		t.Fatalf("subtitle = nil, want %s fallback", subtitleType)
	}
	if got[0].Cache == nil {
		t.Fatal("subtitle cache = nil, want initialized cache")
	}

	parsed := mustTestURL(t, got[0].URL)
	wantPath := "/emby/Videos/" + itemID + "/" + sourceID + "/Subtitles/" + strconv.FormatUint(index, 10) + "/Stream." + subtitleType
	if parsed.Path != wantPath {
		t.Fatalf("subtitle path = %q, want %q", parsed.Path, wantPath)
	}
	assertCanonicalEmbyAPIKey(t, parsed.Query(), url.Values{"api_key": {testEmbyAPIKey}})
	if got[0].Type != subtitleType || got[0].ContentType != contentType {
		t.Fatalf("subtitle format = (%q, %q), want (%q, %q)", got[0].Type, got[0].ContentType, subtitleType, contentType)
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

func TestProcessEmbySubtitlesUsesSafeDeliveryURL(t *testing.T) {
	const streamIndex = uint64(7)
	tests := []struct {
		name            string
		deliveryURL     string
		isText          bool
		deliveryMethod  string
		codec           string
		mimeType        string
		wantScheme      string
		wantHost        string
		wantPath        string
		wantQuery       url.Values
		wantType        string
		wantContentType string
	}{
		{
			name:            "root relative Videos gets emby prefix",
			deliveryURL:     "/Videos/real-item/real-source/Subtitles/91/Stream.srt?foo=keep&API_KEY=old-a&api_Key=old-b&X-EMBY-TOKEN=old-c",
			isText:          false,
			deliveryMethod:  "External",
			wantScheme:      "https",
			wantHost:        "emby.example",
			wantPath:        "/emby/Videos/real-item/real-source/Subtitles/91/Stream.srt",
			wantQuery:       url.Values{"api_key": {testEmbyAPIKey}, "foo": {"keep"}},
			wantType:        "srt",
			wantContentType: "application/x-subrip; charset=utf-8",
		},
		{
			name:            "root relative already under emby is not duplicated",
			deliveryURL:     "/emby/Videos/prefixed-item/prefixed-source/Subtitles/17/Stream.srt?foo=prefixed&api_key=old",
			isText:          true,
			deliveryMethod:  "External",
			wantScheme:      "https",
			wantHost:        "emby.example",
			wantPath:        "/emby/Videos/prefixed-item/prefixed-source/Subtitles/17/Stream.srt",
			wantQuery:       url.Values{"api_key": {testEmbyAPIKey}, "foo": {"prefixed"}},
			wantType:        "srt",
			wantContentType: "application/x-subrip; charset=utf-8",
		},
		{
			name:            "exact Videos path gets emby prefix",
			deliveryURL:     "/Videos?mode=exact",
			wantScheme:      "https",
			wantHost:        "emby.example",
			wantPath:        "/emby/Videos",
			wantQuery:       url.Values{"api_key": {testEmbyAPIKey}, "mode": {"exact"}},
			wantType:        "vtt",
			wantContentType: "text/vtt; charset=utf-8",
		},
		{
			name:            "VideosExtra path does not get emby prefix",
			deliveryURL:     "/VideosExtra/stream.srt?mode=boundary",
			wantScheme:      "https",
			wantHost:        "emby.example",
			wantPath:        "/VideosExtra/stream.srt",
			wantQuery:       url.Values{"api_key": {testEmbyAPIKey}, "mode": {"boundary"}},
			wantType:        "srt",
			wantContentType: "application/x-subrip; charset=utf-8",
		},
		{
			name:            "relative with unknown delivery method",
			deliveryURL:     "Subtitles/7/Stream.vtt?foo=one&foo=two&x-emby-token=old",
			isText:          true,
			deliveryMethod:  "FutureMethod",
			wantScheme:      "https",
			wantHost:        "emby.example",
			wantPath:        "/base/path/Subtitles/7/Stream.vtt",
			wantQuery:       url.Values{"api_key": {testEmbyAPIKey}, "foo": {"one", "two"}},
			wantType:        "vtt",
			wantContentType: "text/vtt; charset=utf-8",
		},
		{
			name:            "same origin absolute Videos gets emby prefix",
			deliveryURL:     "https://emby.example:443/Videos/absolute-item/absolute-source/Subtitles/23/Stream.ass?mode=direct&Api_Key=old&X-Emby-Token=old",
			isText:          false,
			deliveryMethod:  "Unknown",
			wantScheme:      "https",
			wantHost:        "emby.example:443",
			wantPath:        "/emby/Videos/absolute-item/absolute-source/Subtitles/23/Stream.ass",
			wantQuery:       url.Values{"api_key": {testEmbyAPIKey}, "mode": {"direct"}},
			wantType:        "ass",
			wantContentType: "text/x-ssa; charset=utf-8",
		},
		{
			name:            "codec wins for unknown extension",
			deliveryURL:     "/subtitles/stream.unknown?format=codec",
			codec:           "subrip",
			mimeType:        "text/vtt",
			wantScheme:      "https",
			wantHost:        "emby.example",
			wantPath:        "/subtitles/stream.unknown",
			wantQuery:       url.Values{"api_key": {testEmbyAPIKey}, "format": {"codec"}},
			wantType:        "srt",
			wantContentType: "application/x-subrip; charset=utf-8",
		},
		{
			name:            "MIME type wins after unknown extension and codec",
			deliveryURL:     "/subtitles/stream.unknown?format=mime",
			codec:           "unknown",
			mimeType:        "text/vtt; charset=utf-8",
			wantScheme:      "https",
			wantHost:        "emby.example",
			wantPath:        "/subtitles/stream.unknown",
			wantQuery:       url.Values{"api_key": {testEmbyAPIKey}, "format": {"mime"}},
			wantType:        "vtt",
			wantContentType: "text/vtt; charset=utf-8",
		},
		{
			name:            "extension wins over codec and MIME type",
			deliveryURL:     "/subtitles/stream.ass?format=extension",
			codec:           "srt",
			mimeType:        "text/vtt",
			wantScheme:      "https",
			wantHost:        "emby.example",
			wantPath:        "/subtitles/stream.ass",
			wantQuery:       url.Values{"api_key": {testEmbyAPIKey}, "format": {"extension"}},
			wantType:        "ass",
			wantContentType: "text/x-ssa; charset=utf-8",
		},
		{
			name:            "unknown format inputs use controlled VTT default",
			deliveryURL:     "/subtitles/stream.unknown?format=default",
			codec:           "unknown",
			mimeType:        "application/x-unknown",
			wantScheme:      "https",
			wantHost:        "emby.example",
			wantPath:        "/subtitles/stream.unknown",
			wantQuery:       url.Values{"api_key": {testEmbyAPIKey}, "format": {"default"}},
			wantType:        "vtt",
			wantContentType: "text/vtt; charset=utf-8",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := mustTestURL(t, "https://emby.example/base/path/?existing=value&API_KEY=base-stale")
			original := base.String()
			got := processEmbySubtitles(
				&emby.MediaSourceInfo{Id: "fallback-source", MediaStreamInfo: []*emby.MediaStreamInfo{
					{
						Type:                   "Subtitle",
						Index:                  streamIndex,
						DeliveryUrl:            tt.deliveryURL,
						DeliveryMethod:         tt.deliveryMethod,
						Codec:                  tt.codec,
						MimeType:               tt.mimeType,
						IsTextSubtitleStream:   tt.isText,
						SupportsExternalStream: false,
						SubtitleLocationType:   "Unknown",
					},
				}},
				"fallback-item", testEmbyAPIKey, base,
			)

			if len(got) != 1 || got[0] == nil || got[0].Cache == nil {
				t.Fatalf("subtitles = %#v, want one initialized entry", got)
			}
			parsed := mustTestURL(t, got[0].URL)
			if parsed.Scheme != tt.wantScheme || parsed.Host != tt.wantHost {
				t.Fatalf("delivery origin = %s://%s, want %s://%s", parsed.Scheme, parsed.Host, tt.wantScheme, tt.wantHost)
			}
			if parsed.Path != tt.wantPath {
				t.Fatalf("delivery path = %q, want %q", parsed.Path, tt.wantPath)
			}
			assertCanonicalEmbyAPIKey(t, parsed.Query(), tt.wantQuery)
			if got[0].Type != tt.wantType || got[0].ContentType != tt.wantContentType {
				t.Fatalf("subtitle format = (%q, %q), want (%q, %q)", got[0].Type, got[0].ContentType, tt.wantType, tt.wantContentType)
			}
			if base.String() != original {
				t.Fatalf("base URL mutated: got %q, want %q", base, original)
			}
		})
	}
}

func TestProcessEmbySubtitlesFallsBackForUnsafeDeliveryURLWithoutHidingEntry(t *testing.T) {
	const streamIndex = uint64(7)
	tests := []struct {
		name        string
		deliveryURL string
	}{
		{name: "empty", deliveryURL: ""},
		{name: "parse error", deliveryURL: "%"},
		{name: "query parse error", deliveryURL: "/subtitles/stream.vtt?foo=one;bar=two"},
		{name: "cross scheme", deliveryURL: "http://emby.example:443/subtitles/stream.vtt"},
		{name: "cross host", deliveryURL: "https://other.example/subtitles/stream.vtt"},
		{name: "cross effective port", deliveryURL: "https://emby.example:444/subtitles/stream.vtt"},
		{name: "userinfo", deliveryURL: "https://user:password@emby.example/subtitles/stream.vtt"},
		{name: "fragment", deliveryURL: "/subtitles/stream.vtt#private-fragment"},
		{name: "non HTTP scheme", deliveryURL: "file:///private/subtitles/stream.vtt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := mustTestURL(t, "https://emby.example/base/path?API_KEY=base-stale&keep=base")
			original := base.String()
			got := processEmbySubtitles(
				&emby.MediaSourceInfo{Id: "real-source", MediaStreamInfo: []*emby.MediaStreamInfo{
					{
						Type:                 "Subtitle",
						Index:                streamIndex,
						DeliveryUrl:          tt.deliveryURL,
						DeliveryMethod:       "UnrecognizedMethod",
						Codec:                "vtt",
						IsTextSubtitleStream: false,
					},
				}},
				"real-item", testEmbyAPIKey, base,
			)

			assertSingleEmbyFallback(t, got, "real-item", "real-source", streamIndex, "vtt", "text/vtt; charset=utf-8")
			if base.String() != original {
				t.Fatalf("base URL mutated: got %q, want %q", base, original)
			}
		})
	}
}

func TestProcessEmbySubtitlesRouteDiagnostics(t *testing.T) {
	tests := []struct {
		name         string
		base         string
		deliveryURL  string
		wantSource   string
		wantPresent  bool
		wantAccepted bool
		wantPrefix   bool
		wantFallback bool
	}{
		{"delivery accepted with prefix", "https://emby.example/", "/Videos/item/source/Subtitles/1/Stream.vtt", "delivery_url", true, true, true, true},
		{"delivery accepted without prefix", "https://emby.example/", "/subtitles/one.vtt", "delivery_url", true, true, false, true},
		{"delivery rejected with fallback", "https://emby.example/", "https://other.example/private.vtt", "vtt_fallback", true, false, false, true},
		{"delivery missing with fallback", "https://emby.example/", "", "vtt_fallback", false, false, false, true},
		{"none", "file:///invalid", "", "none", false, false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := processEmbySubtitles(&emby.MediaSourceInfo{Id: "source", MediaStreamInfo: []*emby.MediaStreamInfo{{
				Type: "Subtitle", Index: 1, DeliveryUrl: tt.deliveryURL, Codec: "vtt",
			}}}, "item", testEmbyAPIKey, mustTestURL(t, tt.base))
			if len(got) != 1 {
				t.Fatalf("subtitles = %#v", got)
			}
			subtitle := got[0]
			if subtitle.RouteSource != tt.wantSource || subtitle.DeliveryURLPresent != tt.wantPresent ||
				subtitle.DeliveryURLAccepted != tt.wantAccepted || subtitle.APIPrefixAdded != tt.wantPrefix ||
				subtitle.FallbackAvailable != tt.wantFallback {
				t.Fatalf("route diagnostics = %#v", subtitle)
			}
			if tt.wantSource == "none" {
				_, err := subtitle.Cache.Get(context.Background())
				details, ok := EmbyDiagnosticDetailsFromError(err)
				if !ok || details.RouteSource != tt.wantSource || details.DeliveryURLPresent != tt.wantPresent ||
					details.DeliveryURLAccepted != tt.wantAccepted || details.APIPrefixAdded != tt.wantPrefix ||
					details.FallbackAvailable != tt.wantFallback {
					t.Fatalf("error route diagnostics = %#v, ok=%v", details, ok)
				}
				assertRedactedSubtitleError(t, err, []string{"api_key", testEmbyAPIKey, "item", "source"})
			}
		})
	}
}

func TestEmbySubtitleCacheGetPreservesRouteMetadataFromRealUpstream404(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer server.Close()

	got := processEmbySubtitles(&emby.MediaSourceInfo{Id: "source", MediaStreamInfo: []*emby.MediaStreamInfo{{
		Type: "Subtitle", Index: 1, DeliveryUrl: "/subtitle.vtt", Codec: "vtt",
	}}}, "item", testEmbyAPIKey, mustTestURL(t, server.URL))
	if len(got) != 1 || got[0] == nil || got[0].Cache == nil {
		t.Fatalf("subtitles = %#v, want one initialized entry", got)
	}

	_, err := got[0].Cache.Get(context.Background())
	details, ok := EmbyDiagnosticDetailsFromError(err)
	if !ok || details.Category != "subtitle_upstream_status" || details.HTTPStatus != http.StatusNotFound ||
		details.RouteSource != "delivery_url" || !details.DeliveryURLPresent || !details.DeliveryURLAccepted ||
		details.APIPrefixAdded || !details.FallbackAvailable {
		t.Fatalf("404 diagnostic details = %#v, ok = %v", details, ok)
	}
}

func TestProcessEmbySubtitlesKeepsEntryWithoutSelectableUpstreamURL(t *testing.T) {
	got := processEmbySubtitles(
		&emby.MediaSourceInfo{Id: "source", MediaStreamInfo: []*emby.MediaStreamInfo{
			{Type: "Subtitle", Index: 7, DeliveryUrl: "https://other.example/private.vtt?api_key=secret"},
		}},
		"item", testEmbyAPIKey, mustTestURL(t, "file:///invalid-base"),
	)

	if len(got) != 1 || got[0] == nil {
		t.Fatalf("subtitles = %#v, want one selector entry", got)
	}
	if got[0].URL != "" {
		t.Fatalf("subtitle URL = %q, want empty", got[0].URL)
	}
	if got[0].Cache == nil {
		t.Fatal("subtitle cache = nil, want initialized cache")
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

func TestProcessEmbySubtitlesFallbackUsesSupportedTextFormatAndDoesNotMutateBase(t *testing.T) {
	base := mustTestURL(t, "https://emby.example/base/path?original=value")
	original := base.String()
	tests := []struct {
		name            string
		codec           string
		mimeType        string
		wantType        string
		wantContentType string
	}{
		{"VTT codec", "webvtt", "application/x-subrip", "vtt", "text/vtt; charset=utf-8"},
		{"SRT codec", "subrip", "text/vtt", "srt", "application/x-subrip; charset=utf-8"},
		{"ASS codec", "ssa", "text/vtt", "ass", "text/x-ssa; charset=utf-8"},
		{"MIME fallback", "unknown", "application/subrip; charset=utf-8", "srt", "application/x-subrip; charset=utf-8"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			index := uint64(i + 2)
			got := processEmbySubtitles(
				&emby.MediaSourceInfo{Id: "source", MediaStreamInfo: []*emby.MediaStreamInfo{{
					Type: "Subtitle", Codec: tt.codec, MimeType: tt.mimeType, Index: index,
				}}},
				"item", testEmbyAPIKey, base,
			)
			assertSingleEmbyFallback(t, got, "item", "source", index, tt.wantType, tt.wantContentType)
			if got[0].RouteSource != "vtt_fallback" || !got[0].FallbackAvailable {
				t.Fatalf("fallback route = %#v", got[0])
			}
		})
	}
	if base.String() != original {
		t.Fatalf("base URL mutated: got %q, want %q", base, original)
	}
}

func TestProcessEmbySubtitlesUnknownFallbackFormatFailsClosed(t *testing.T) {
	got := processEmbySubtitles(&emby.MediaSourceInfo{Id: "source", MediaStreamInfo: []*emby.MediaStreamInfo{
		{Type: "Subtitle", Codec: "pgssub", MimeType: "image/png", Index: 7},
	}}, "item", testEmbyAPIKey, mustTestURL(t, "https://emby.example"))
	if len(got) != 1 || got[0] == nil || got[0].URL != "" || got[0].FallbackAvailable || got[0].RouteSource != "none" {
		t.Fatalf("unknown subtitle fallback = %#v, want retained fail-closed entry", got)
	}
	_, err := got[0].Cache.Get(context.Background())
	if category := EmbyDiagnosticErrorCategory(err); category != "subtitle_cache_fetch_failed" {
		t.Fatalf("category = %q, want subtitle_cache_fetch_failed", category)
	}
}

func TestEmbySubtitleCacheGetFetchesCodecSpecificFallback(t *testing.T) {
	tests := []struct {
		name            string
		codec           string
		wantPath        string
		wantType        string
		wantContentType string
		body            string
	}{
		{
			name:            "subrip",
			codec:           "subrip",
			wantPath:        "/emby/Videos/item/source/Subtitles/7/Stream.srt",
			wantType:        "srt",
			wantContentType: "application/x-subrip; charset=utf-8",
			body:            "1\n00:00:00,000 --> 00:00:01,000\nSRT fallback\n",
		},
		{
			name:            "ass",
			codec:           "ass",
			wantPath:        "/emby/Videos/item/source/Subtitles/7/Stream.ass",
			wantType:        "ass",
			wantContentType: "text/x-ssa; charset=utf-8",
			body:            "[Script Info]\nTitle: ASS fallback\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tt.wantPath {
					t.Errorf("subtitle request path = %q, want %q", r.URL.Path, tt.wantPath)
				}
				assertCanonicalEmbyAPIKey(t, r.URL.Query(), url.Values{"api_key": {testEmbyAPIKey}})
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			got := processEmbySubtitles(&emby.MediaSourceInfo{Id: "source", MediaStreamInfo: []*emby.MediaStreamInfo{{
				Type: "Subtitle", Index: 7, Codec: tt.codec,
			}}}, "item", testEmbyAPIKey, mustTestURL(t, server.URL))
			if len(got) != 1 || got[0] == nil || got[0].Cache == nil {
				t.Fatalf("subtitles = %#v, want one initialized fallback", got)
			}
			if got[0].Type != tt.wantType || got[0].ContentType != tt.wantContentType || got[0].RouteSource != "vtt_fallback" {
				t.Fatalf("fallback metadata = %#v", got[0])
			}

			body, err := got[0].Cache.Get(context.Background())
			if err != nil {
				t.Fatalf("fetch fallback subtitle: %v", err)
			}
			if string(body) != tt.body {
				t.Fatalf("subtitle body = %q, want %q", body, tt.body)
			}
		})
	}
}

func TestEmbySubtitleCacheGetAcceptsSRTDeliveryURLForUnknownGraphicCodec(t *testing.T) {
	const body = "1\n00:00:00,000 --> 00:00:01,000\nDelivery subtitle\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/subtitle.srt" {
			t.Errorf("subtitle request path = %q, want /subtitle.srt", r.URL.Path)
		}
		assertCanonicalEmbyAPIKey(t, r.URL.Query(), url.Values{"api_key": {testEmbyAPIKey}})
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	got := processEmbySubtitles(&emby.MediaSourceInfo{Id: "source", MediaStreamInfo: []*emby.MediaStreamInfo{{
		Type: "Subtitle", Index: 7, Codec: "pgssub", MimeType: "image/png", DeliveryUrl: "/subtitle.srt",
	}}}, "item", testEmbyAPIKey, mustTestURL(t, server.URL))
	if len(got) != 1 || got[0] == nil || got[0].Cache == nil {
		t.Fatalf("subtitles = %#v, want one initialized delivery entry", got)
	}
	if got[0].Type != "srt" || got[0].ContentType != "application/x-subrip; charset=utf-8" ||
		got[0].RouteSource != "delivery_url" || !got[0].DeliveryURLAccepted {
		t.Fatalf("delivery metadata = %#v", got[0])
	}

	data, err := got[0].Cache.Get(context.Background())
	if err != nil {
		t.Fatalf("fetch delivery subtitle: %v", err)
	}
	if string(data) != body {
		t.Fatalf("subtitle body = %q, want %q", data, body)
	}
}

func TestProcessPlaybackInfoResponseKeepsSubtitleOrderAndRealUpstreamIdentifiers(t *testing.T) {
	const requestedItemID = "requested-item"
	binding := &EmbyUserCacheData{
		Host:   "https://emby.example/library/",
		APIKey: testEmbyAPIKey,
	}
	response := &emby.PlaybackInfoResp{
		MediaSourceInfo: []*emby.MediaSourceInfo{
			nil,
			{
				Id:            "source-alpha",
				DirectPlayUrl: "/Videos/alpha/stream",
				MediaStreamInfo: []*emby.MediaStreamInfo{
					nil,
					{Type: "Audio", Index: 20},
					{Type: "Subtitle", Index: 41, DisplayTitle: "Alpha delivery", DeliveryUrl: "/metadata/alpha.srt?keep=alpha"},
					{Type: "Subtitle", Index: 43, DisplayTitle: "Alpha fallback", DeliveryUrl: "https://other.example/unsafe.vtt", Codec: "vtt"},
				},
			},
			{
				Id:            "source-beta",
				DirectPlayUrl: "/Videos/beta/stream",
				MediaStreamInfo: []*emby.MediaStreamInfo{
					{Type: "subtitle", Index: 50},
					{Type: "Subtitle", Index: 73, DisplayTitle: "Beta delivery", DeliveryUrl: "relative/beta.vtt"},
				},
			},
		},
	}

	got, err := processPlaybackInfoResponse(response, nil, binding, requestedItemID)
	if err != nil {
		t.Fatalf("process playback info: %v", err)
	}
	if got == nil || len(got.Sources) != 3 {
		t.Fatalf("sources = %#v, want three stable source slots", got)
	}
	if got.Sources[0].URL != "" {
		t.Fatalf("nil source slot URL = %q, want empty", got.Sources[0].URL)
	}
	if len(got.Sources[1].Subtitles) != 2 || len(got.Sources[2].Subtitles) != 1 {
		t.Fatalf("subtitle counts = (%d, %d), want (2, 1)", len(got.Sources[1].Subtitles), len(got.Sources[2].Subtitles))
	}

	ordered := []*EmbySubtitleCache{
		got.Sources[1].Subtitles[0],
		got.Sources[1].Subtitles[1],
		got.Sources[2].Subtitles[0],
	}
	wantNames := []string{"Alpha delivery", "Alpha fallback", "Beta delivery"}
	for i, subtitle := range ordered {
		if subtitle == nil || subtitle.Cache == nil || subtitle.Name != wantNames[i] {
			t.Fatalf("subtitle %d = %#v, want initialized %q entry", i, subtitle, wantNames[i])
		}
	}

	alphaDelivery := mustTestURL(t, ordered[0].URL)
	if alphaDelivery.Path != "/metadata/alpha.srt" {
		t.Fatalf("alpha DeliveryUrl path = %q, want metadata path independent of source/stream array positions", alphaDelivery.Path)
	}
	assertCanonicalEmbyAPIKey(t, alphaDelivery.Query(), url.Values{
		"api_key": {testEmbyAPIKey},
		"keep":    {"alpha"},
	})

	alphaFallback := mustTestURL(t, ordered[1].URL)
	if alphaFallback.Path != "/emby/Videos/requested-item/source-alpha/Subtitles/43/Stream.vtt" {
		t.Fatalf("alpha fallback path = %q, want real item/source/stream identifiers", alphaFallback.Path)
	}
	assertCanonicalEmbyAPIKey(t, alphaFallback.Query(), url.Values{"api_key": {testEmbyAPIKey}})

	betaDelivery := mustTestURL(t, ordered[2].URL)
	if betaDelivery.Path != "/library/relative/beta.vtt" {
		t.Fatalf("beta DeliveryUrl path = %q, want path independent of source/stream array positions", betaDelivery.Path)
	}
	assertCanonicalEmbyAPIKey(t, betaDelivery.Query(), url.Values{"api_key": {testEmbyAPIKey}})
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

func TestNewEmbySubtitleCacheInitFuncClassifiesEmptyURLSafely(t *testing.T) {
	data, err := newEmbySubtitleCacheInitFunc("")(context.Background())
	if data != nil {
		t.Fatalf("subtitle data = %#v, want nil", data)
	}
	if category := EmbyDiagnosticErrorCategory(err); category != "subtitle_cache_fetch_failed" {
		t.Fatalf("category = %q, want subtitle_cache_fetch_failed", category)
	}
	if err == nil || err.Error() != "subtitle cache fetch failed" {
		t.Fatalf("error = %v, want fixed safe error", err)
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
