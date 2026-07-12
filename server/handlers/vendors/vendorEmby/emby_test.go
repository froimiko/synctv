package vendoremby

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/synctv-org/synctv/internal/cache"
	dbModel "github.com/synctv-org/synctv/internal/model"
	"github.com/synctv-org/synctv/internal/op"
	"github.com/synctv-org/synctv/server/model"
)

func TestDynamicMovieEmbyCredentialsFromCache(t *testing.T) {
	const serverID = "creator-server"

	creatorCache := cache.NewEmbyUserCache("creator")
	if _, err := creatorCache.LoadOrStoreWithDynamicFunc(
		context.Background(),
		serverID,
		func(context.Context, string) (*cache.EmbyUserCacheData, error) {
			return &cache.EmbyUserCacheData{
				Host:     "https://creator.example",
				ServerID: serverID,
				APIKey:   "creator-api-key",
				UserID:   "creator-emby-user",
				Backend:  "creator-backend",
			}, nil
		},
	); err != nil {
		t.Fatalf("seed creator cache: %v", err)
	}

	credentials, err := dynamicMovieEmbyCredentialsFromCache(
		context.Background(), creatorCache, serverID,
	)
	if err != nil {
		t.Fatalf("load dynamic movie credentials: %v", err)
	}
	if credentials.host != "https://creator.example" {
		t.Fatalf("host mismatch")
	}
	if credentials.apiKey != "creator-api-key" {
		t.Fatalf("API key mismatch")
	}
	if credentials.userID != "creator-emby-user" {
		t.Fatalf("user ID mismatch")
	}
	if credentials.backend != "creator-backend" {
		t.Fatalf("backend mismatch")
	}
}

func TestProxyMovieRejectsNegativeSubtitleIndexes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name  string
		query string
		error string
	}{
		{
			name:  "negative source",
			query: "t=subtitle&source=-1&id=0",
			error: "invalid subtitle query: source out of range",
		},
		{
			name:  "negative id",
			query: "t=subtitle&source=0&id=-1",
			error: "invalid subtitle query: id out of range",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/?"+tt.query, nil)

			service := &EmbyVendorService{}
			service.ProxyMovie(ctx)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
			}

			var resp model.APIResp
			if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp.Error != tt.error {
				t.Fatalf("error = %q, want %q", resp.Error, tt.error)
			}
			if strings.Contains(recorder.Body.String(), "api_key") {
				t.Fatalf("response leaked sensitive upstream details: %s", recorder.Body.String())
			}
		})
	}
}

func TestEmbyProxyDecisions(t *testing.T) {
	creator := &op.User{User: dbModel.User{ID: "creator"}}
	guest := &op.User{User: dbModel.User{ID: "guest"}}

	directMovie := &op.Movie{Movie: &dbModel.Movie{
		CreatorID: "creator",
		MovieBase: dbModel.MovieBase{Proxy: false},
	}}
	proxyMovie := &op.Movie{Movie: &dbModel.Movie{
		CreatorID: "creator",
		MovieBase: dbModel.MovieBase{Proxy: true},
	}}

	if shouldProxyEmbyMovie(directMovie, creator) {
		t.Fatal("creator should keep direct playback when proxy is disabled")
	}
	if !shouldProxyEmbyMovie(directMovie, guest) {
		t.Fatal("non-creator must use the SyncTV proxy")
	}
	if !shouldProxyEmbyMovie(proxyMovie, creator) {
		t.Fatal("proxy-enabled movie must use the SyncTV proxy")
	}
	if !shouldProxyEmbyMovie(directMovie, nil) {
		t.Fatal("missing user must not receive an upstream URL")
	}

	if canRequestEmbyProxy(directMovie, creator) {
		t.Fatal("creator must not use a disabled proxy endpoint")
	}
	if !canRequestEmbyProxy(directMovie, guest) {
		t.Fatal("non-creator must be allowed to use the internal proxy")
	}
	if !canRequestEmbyProxy(proxyMovie, creator) {
		t.Fatal("creator must be allowed when proxy is enabled")
	}
	if canRequestEmbyProxy(directMovie, nil) {
		t.Fatal("missing user must fail closed")
	}
}

func TestEmbySourceCacheKeyTreatsTranscodeAsProxySource(t *testing.T) {
	const upstream = "https://creator.example/emby/video.m3u8?api_key=secret&DeviceId=device&PlaySessionId=session"

	for _, isTranscode := range []bool{false, true} {
		got, err := embySourceCacheKey(cache.EmbySource{URL: upstream, IsTranscode: isTranscode})
		if err != nil {
			t.Fatalf("build source cache key: %v", err)
		}
		parsed, err := url.Parse(got)
		if err != nil {
			t.Fatalf("parse source cache key: %v", err)
		}
		if parsed.Query().Get("DeviceId") != "" || parsed.Query().Get("PlaySessionId") != "" {
			t.Fatalf("volatile session query remained: %q", got)
		}
		if parsed.Query().Get("api_key") != "secret" {
			t.Fatalf("authorization query unexpectedly changed: %q", got)
		}
	}
}

func TestEmbyMovieProxyURLContainsOnlyInternalAuthorization(t *testing.T) {
	rawURL, err := embyMovieProxyURL("movie-id", "room-id", "member-token", 3)
	if err != nil {
		t.Fatalf("build movie proxy URL: %v", err)
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse movie proxy URL: %v", err)
	}
	if parsed.Path != "/api/room/movie/proxy/movie-id" {
		t.Fatalf("path = %q", parsed.Path)
	}
	query := parsed.Query()
	if query.Get("source") != "3" || query.Get("token") != "member-token" || query.Get("roomId") != "room-id" {
		t.Fatalf("query = %q", parsed.RawQuery)
	}
	for _, sensitive := range []string{"api_key", "creator.example", "https://"} {
		if strings.Contains(rawURL, sensitive) {
			t.Fatalf("internal proxy URL leaked upstream detail %q: %q", sensitive, rawURL)
		}
	}
}

func TestEmbySubtitleProxyURLUsesPerUserToken(t *testing.T) {
	const (
		movieID = "movie-id"
		roomID  = "room-id"
	)

	ownerURL, err := embySubtitleProxyURL(movieID, roomID, "owner-token", 1, 2)
	if err != nil {
		t.Fatalf("build owner subtitle URL: %v", err)
	}
	memberURL, err := embySubtitleProxyURL(movieID, roomID, "member-token", 1, 2)
	if err != nil {
		t.Fatalf("build member subtitle URL: %v", err)
	}

	for name, rawURL := range map[string]string{"owner": ownerURL, "member": memberURL} {
		parsed, err := url.Parse(rawURL)
		if err != nil {
			t.Fatalf("parse %s URL: %v", name, err)
		}
		if parsed.Path != "/api/room/movie/proxy/"+movieID {
			t.Errorf("%s path = %q", name, parsed.Path)
		}
		query := parsed.Query()
		if query.Get("t") != "subtitle" || query.Get("source") != "1" || query.Get("id") != "2" || query.Get("roomId") != roomID {
			t.Errorf("%s query = %q", name, parsed.RawQuery)
		}
		if strings.Contains(rawURL, "api_key") {
			t.Errorf("%s URL leaked api_key: %q", name, rawURL)
		}
	}

	if got := mustParseQuery(t, ownerURL).Get("token"); got != "owner-token" {
		t.Fatalf("owner token = %q", got)
	}
	if got := mustParseQuery(t, memberURL).Get("token"); got != "member-token" {
		t.Fatalf("member token = %q", got)
	}
}

func TestRebuildEmbyProxyMovieClearsStaleFieldsAndUsesFirstValidSource(t *testing.T) {
	movie := &dbModel.Movie{
		ID:     "movie-id",
		RoomID: "room-id",
		MovieBase: dbModel.MovieBase{
			URL:  "https://creator.example/stale.mp4?api_key=secret",
			Type: "stale",
			Headers: map[string]string{
				"Authorization": "secret",
			},
			MoreSources: []*dbModel.MoreSource{
				{URL: "https://creator.example/stale-more.mp4?api_key=secret"},
			},
			Subtitles: map[string]*dbModel.Subtitle{
				"stale": {URL: "https://creator.example/stale.vtt?api_key=secret"},
			},
		},
	}
	sources := []cache.EmbySource{
		{URL: "", Name: "empty", Subtitles: []*cache.EmbySubtitleCache{{Name: "ignored", URL: "https://creator.example/ignored.vtt?api_key=secret"}}},
		{
			URL:  "https://creator.example/video.m3u8?api_key=secret",
			Name: "primary",
			Subtitles: []*cache.EmbySubtitleCache{
				{Name: "English", Type: "vtt", URL: "https://creator.example/subtitle.vtt?api_key=secret"},
			},
		},
		{URL: "https://creator.example/backup.mp4?api_key=secret", Name: "backup"},
	}

	got, err := rebuildEmbyProxyMovie(movie, sources, "member-token")
	if err != nil {
		t.Fatalf("rebuild proxy movie: %v", err)
	}

	if got.Headers != nil {
		t.Fatal("stale headers were not cleared")
	}
	if got.Type != "m3u8" {
		t.Fatalf("type = %q, want m3u8", got.Type)
	}
	if query := mustParseQuery(t, got.URL); query.Get("source") != "1" {
		t.Fatalf("primary source query = %q", query.Encode())
	}
	if len(got.MoreSources) != 1 || mustParseQuery(t, got.MoreSources[0].URL).Get("source") != "2" {
		t.Fatalf("more sources = %#v", got.MoreSources)
	}
	if len(got.Subtitles) != 1 || got.Subtitles["English"] == nil {
		t.Fatalf("subtitles = %#v", got.Subtitles)
	}
	if query := mustParseQuery(t, got.Subtitles["English"].URL); query.Get("source") != "1" || query.Get("id") != "0" {
		t.Fatalf("subtitle query = %q", query.Encode())
	}

	output, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal proxy movie: %v", err)
	}
	for _, sensitive := range []string{"creator.example", "api_key", "secret"} {
		if strings.Contains(string(output), sensitive) {
			t.Fatalf("proxy output leaked %q: %q", sensitive, output)
		}
	}
}

func TestRebuildEmbyProxyMovieRejectsNilMovie(t *testing.T) {
	got, err := rebuildEmbyProxyMovie(nil, []cache.EmbySource{{URL: "https://creator.example/video.mp4"}}, "member-token")
	if err == nil || err.Error() != "movie is required" {
		t.Fatalf("error = %v, want movie is required", err)
	}
	if got != nil {
		t.Fatalf("movie = %#v, want nil", got)
	}
}

func TestRebuildEmbyProxyMovieRejectsMissingSources(t *testing.T) {
	movie := &dbModel.Movie{
		ID:     "movie-id",
		RoomID: "room-id",
		MovieBase: dbModel.MovieBase{
			URL:         "https://creator.example/stale.mp4?api_key=secret",
			Headers:     map[string]string{"Authorization": "secret"},
			MoreSources: []*dbModel.MoreSource{{URL: "https://creator.example/stale-more.mp4"}},
			Subtitles:   map[string]*dbModel.Subtitle{"stale": {URL: "https://creator.example/stale.vtt"}},
		},
	}

	got, err := rebuildEmbyProxyMovie(movie, []cache.EmbySource{{Name: "empty"}}, "member-token")
	if err == nil || err.Error() != "no source" {
		t.Fatalf("error = %v, want no source", err)
	}
	if got != nil {
		t.Fatalf("movie = %#v, want nil", got)
	}
	if movie.URL != "" || movie.MoreSources != nil || movie.Headers != nil || movie.Subtitles != nil {
		t.Fatalf("stale fields remained after rejection: %#v", movie.MovieBase)
	}
}

func mustParseQuery(t *testing.T, rawURL string) url.Values {
	t.Helper()

	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	return parsed.Query()
}
