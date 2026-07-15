package vendoremby

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/synctv-org/synctv/internal/cache"
	"github.com/synctv-org/synctv/internal/db"
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

func TestHandleEmbySubtitleChecksGrantBeforeIndexesAndFetch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, grantErr := range []error{db.NewEmbyGrantError("not_found"), errors.New("grant backend failed")} {
		for _, rawQuery := range []string{"source=bad&id=0", "source=-1&id=-1", "source=99&id=99", "source=0&source=1&id=0"} {
			t.Run(grantErr.Error()+"/"+rawQuery, func(t *testing.T) {
				ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
				ctx.Request = httptest.NewRequest(http.MethodGet, "/?"+rawQuery, nil)
				values, err := url.ParseQuery(ctx.Request.URL.RawQuery)
				if err != nil {
					t.Fatalf("parse test query: %v", err)
				}
				fetchCalled := false
				err = handleEmbySubtitle(ctx, values,
					func(context.Context) (*cache.EmbyMovieCacheData, error) { return nil, grantErr },
					func(context.Context, *cache.EmbySubtitleCache) ([]byte, error) {
						fetchCalled = true
						return nil, nil
					},
				)
				if !errors.Is(err, grantErr) || fetchCalled {
					t.Fatalf("err = %v, fetchCalled = %v", err, fetchCalled)
				}
			})
		}
	}
}

func TestProxyMovieRejectsInvalidQuerySyntaxAndRepeatedType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	queries := []string{
		"t=subtitle&t=subtitle&source=0&id=0",
		"t=subtitle&source=%zz&id=0",
		"t=subtitle&source=0&id=0;extra=x",
		"t=subtitle&source=0&id=0&extra=%zz",
	}
	for _, query := range queries {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/?"+query, nil)
		(&EmbyVendorService{}).ProxyMovie(ctx)
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("query %q: status = %d, want 400", query, recorder.Code)
		}
	}
}

func TestHandleEmbySubtitleRejectsRepeatedIndexesAfterGrant(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cached := &cache.EmbyMovieCacheData{Sources: []cache.EmbySource{{Subtitles: []*cache.EmbySubtitleCache{{}}}}}
	for _, rawQuery := range []string{"source=0&source=1&id=0", "source=0&id=0&id=1"} {
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodGet, "/?"+rawQuery, nil)
		values, err := url.ParseQuery(ctx.Request.URL.RawQuery)
		if err != nil {
			t.Fatalf("parse query: %v", err)
		}
		fetchCalled := false
		err = handleEmbySubtitle(ctx, values,
			func(context.Context) (*cache.EmbyMovieCacheData, error) { return cached, nil },
			func(context.Context, *cache.EmbySubtitleCache) ([]byte, error) {
				fetchCalled = true
				return nil, nil
			},
		)
		if !errors.Is(err, errInvalidSubtitleQuery) || fetchCalled {
			t.Errorf("query %q: err = %v, fetchCalled = %v", rawQuery, err, fetchCalled)
		}
	}
}

func TestServeEmbySubtitleSetsDeclaredContentType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name        string
		contentType string
	}{
		{name: "vtt", contentType: "text/vtt; charset=utf-8"},
		{name: "srt", contentType: "application/x-subrip; charset=utf-8"},
		{name: "ass", contentType: "text/x-ssa; charset=utf-8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/subtitle", nil)

			serveEmbySubtitle(ctx, &cache.EmbySubtitleCache{
				Name:        "subtitle." + tt.name,
				ContentType: tt.contentType,
			}, []byte("subtitle body"))

			if got := recorder.Header().Get("Content-Type"); got != tt.contentType {
				t.Fatalf("Content-Type = %q, want %q", got, tt.contentType)
			}
			if recorder.Code != http.StatusOK || recorder.Body.String() != "subtitle body" {
				t.Fatalf("response = (%d, %q)", recorder.Code, recorder.Body.String())
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
	if shouldProxyEmbyMovie(directMovie, guest) {
		t.Fatal("guest should keep direct playback when proxy is disabled")
	}
	if !shouldProxyEmbyMovie(proxyMovie, creator) {
		t.Fatal("proxy-enabled movie must use the SyncTV proxy")
	}
	if !shouldProxyEmbyMovie(proxyMovie, guest) {
		t.Fatal("proxy-enabled movie must use the SyncTV proxy for guests")
	}
	if !shouldProxyEmbyMovie(directMovie, nil) {
		t.Fatal("missing user must not receive an upstream URL")
	}

	if canRequestEmbyProxy(directMovie, creator) {
		t.Fatal("creator must not use a disabled proxy endpoint")
	}
	if canRequestEmbyProxy(directMovie, guest) {
		t.Fatal("guest must not use a disabled proxy endpoint")
	}
	if !canRequestEmbyProxy(proxyMovie, creator) {
		t.Fatal("creator must be allowed when proxy is enabled")
	}
	if !canRequestEmbyProxy(proxyMovie, guest) {
		t.Fatal("guest must be allowed when proxy is enabled")
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

func TestEmbySubtitleProxyURLContractIsIndependentOfRoleAndMovieProxy(t *testing.T) {
	const (
		movieID = "movie-id"
		roomID  = "room-id"
	)
	tests := []struct {
		name  string
		proxy bool
		token string
	}{
		{name: "owner direct movie", proxy: false, token: "owner-token"},
		{name: "owner proxy movie", proxy: true, token: "owner-token"},
		{name: "guest direct movie", proxy: false, token: "guest-token"},
		{name: "guest proxy movie", proxy: true, token: "guest-token"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			movie := &dbModel.Movie{
				ID:        movieID,
				RoomID:    roomID,
				MovieBase: dbModel.MovieBase{Proxy: tt.proxy},
			}
			source := cache.EmbySource{Subtitles: []*cache.EmbySubtitleCache{
				nil,
				nil,
				{Name: "English", Type: "vtt", URL: "https://upstream.invalid/subtitle.vtt?api_key=secret"},
			}}
			if err := addEmbySourceSubtitles(movie, source, tt.token, 1); err != nil {
				t.Fatalf("add subtitles: %v", err)
			}
			rawURL := movie.Subtitles["English"].URL
			parsed, err := url.Parse(rawURL)
			if err != nil {
				t.Fatalf("parse subtitle URL: %v", err)
			}
			if parsed.Path != "/api/room/movie/proxy/"+movieID {
				t.Errorf("path = %q", parsed.Path)
			}
			query := parsed.Query()
			if query.Get("t") != "subtitle" || query.Get("source") != "1" || query.Get("id") != "2" ||
				query.Get("roomId") != roomID || query.Get("token") != tt.token {
				t.Errorf("query = %q", parsed.RawQuery)
			}
			for _, sensitive := range []string{"api_key", "upstream.invalid", "secret"} {
				if strings.Contains(rawURL, sensitive) {
					t.Errorf("URL leaked %q: %q", sensitive, rawURL)
				}
			}
		})
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

func TestAddEmbySourceSubtitlesPreservesDuplicateAndEmptyNames(t *testing.T) {
	movie := &dbModel.Movie{ID: "movie-id", RoomID: "room-id"}
	sources := []cache.EmbySource{
		{
			URL: "https://upstream.invalid/primary.mp4?api_key=source-secret",
			Subtitles: []*cache.EmbySubtitleCache{
				{Name: "English", Type: "vtt", URL: "https://upstream.invalid/sub-0.vtt?api_key=subtitle-secret"},
				{Name: "English", Type: "srt", URL: "https://upstream.invalid/sub-1.srt?api_key=subtitle-secret"},
				{Name: "", Type: "ass", URL: "https://upstream.invalid/sub-2.ass?api_key=subtitle-secret"},
				{Name: "", Type: "ass", URL: "https://upstream.invalid/sub-3.ass?api_key=subtitle-secret"},
			},
		},
		{
			URL: "https://upstream.invalid/secondary.mp4?api_key=source-secret",
			Subtitles: []*cache.EmbySubtitleCache{
				{Name: "English", Type: "vtt", URL: "https://upstream.invalid/sub-4.vtt?api_key=subtitle-secret"},
			},
		},
	}

	for sourceIndex, source := range sources {
		if err := addEmbySourceSubtitles(movie, source, "member-token", sourceIndex); err != nil {
			t.Fatalf("add source %d subtitles: %v", sourceIndex, err)
		}
	}

	wantQueries := map[string][2]string{
		"English":                              {"0", "0"},
		"English (srt, source 1, subtitle 2)":  {"0", "1"},
		"Subtitle":                             {"0", "2"},
		"Subtitle (ass, source 1, subtitle 4)": {"0", "3"},
		"English (vtt, source 2, subtitle 1)":  {"1", "0"},
	}
	if len(movie.Subtitles) != len(wantQueries) {
		t.Fatalf("subtitle count = %d, want %d: %#v", len(movie.Subtitles), len(wantQueries), movie.Subtitles)
	}
	for name, want := range wantQueries {
		subtitle := movie.Subtitles[name]
		if subtitle == nil {
			t.Errorf("missing subtitle %q", name)
			continue
		}
		query := mustParseQuery(t, subtitle.URL)
		if query.Get("source") != want[0] || query.Get("id") != want[1] || query.Get("token") != "member-token" {
			t.Errorf("subtitle %q query = %q", name, query.Encode())
		}
	}

	output, err := json.Marshal(movie.Subtitles)
	if err != nil {
		t.Fatalf("marshal subtitles: %v", err)
	}
	for _, sensitive := range []string{"upstream.invalid", "api_key", "source-secret", "subtitle-secret"} {
		if strings.Contains(string(output), sensitive) {
			t.Fatalf("subtitle output leaked %q: %s", sensitive, output)
		}
	}
}

func TestAddEmbySourceSubtitlesUsesStableCollisionCounter(t *testing.T) {
	movie := &dbModel.Movie{
		ID:     "movie-id",
		RoomID: "room-id",
		MovieBase: dbModel.MovieBase{Subtitles: map[string]*dbModel.Subtitle{
			"English":                             nil,
			"English (vtt, source 1, subtitle 1)": nil,
		}},
	}
	source := cache.EmbySource{Subtitles: []*cache.EmbySubtitleCache{{Name: "English", Type: "vtt"}}}
	if err := addEmbySourceSubtitles(movie, source, "member-token", 0); err != nil {
		t.Fatalf("add subtitles: %v", err)
	}
	if movie.Subtitles["English (vtt, source 1, subtitle 1) #2"] == nil {
		t.Fatalf("collision counter subtitle missing: %#v", movie.Subtitles)
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

func TestLoadAuthorizedEmbyMovieCacheDataOrdersAuthorizationBeforeGet(t *testing.T) {
	creatorCache := cache.NewEmbyUserCache("creator")
	cached := &cache.EmbyMovieCacheData{
		TranscodeSessionID: "transcode-session",
		Sources: []cache.EmbySource{{
			URL: "https://creator.example/video.m3u8?api_key=secret",
			Subtitles: []*cache.EmbySubtitleCache{{
				URL: "https://creator.example/subtitle.vtt?api_key=secret",
			}},
		}},
	}
	now := time.Date(2026, time.July, 13, 0, 0, 0, 0, time.UTC)
	calls := make([]string, 0, 3)

	got, err := loadAuthorizedEmbyMovieCacheData(
		context.Background(),
		&dbModel.Movie{ID: "movie-id"},
		"child-id",
		"creator-id",
		func() time.Time { return now },
		func(_ context.Context, creatorID string) (*cache.EmbyUserCache, error) {
			calls = append(calls, "load")
			if creatorID != "creator-id" {
				t.Fatalf("creator ID = %q", creatorID)
			}
			return creatorCache, nil
		},
		func(_ context.Context, movie *dbModel.Movie, subPath string, gotCache *cache.EmbyUserCache, gotNow time.Time) error {
			calls = append(calls, "validate")
			if movie.ID != "movie-id" || subPath != "child-id" || gotCache != creatorCache || !gotNow.Equal(now) {
				t.Fatal("authorization inputs changed")
			}
			return nil
		},
		func(_ context.Context, gotCache *cache.EmbyUserCache) (*cache.EmbyMovieCacheData, error) {
			calls = append(calls, "get")
			if gotCache != creatorCache {
				t.Fatal("cache getter received a different creator cache")
			}
			return cached, nil
		},
	)
	if err != nil {
		t.Fatalf("load authorized cache: %v", err)
	}
	if got != cached {
		t.Fatal("authorized cache object was not returned")
	}
	if strings.Join(calls, ",") != "load,validate,get" {
		t.Fatalf("call order = %q", calls)
	}
}

func TestLoadAuthorizedEmbyMovieCacheDataRejectsExpiredGrantBeforeCachedExposure(t *testing.T) {
	creatorCache := cache.NewEmbyUserCache("creator")
	grantedAt := time.Date(2026, time.July, 13, 0, 0, 0, 0, time.UTC)
	now := grantedAt.Add(dbModel.EmbyRootGrantLease + time.Second)
	getCalled := false

	got, err := loadAuthorizedEmbyMovieCacheData(
		context.Background(),
		&dbModel.Movie{ID: "movie-id"},
		"child-id",
		"creator-id",
		func() time.Time { return now },
		func(context.Context, string) (*cache.EmbyUserCache, error) {
			return creatorCache, nil
		},
		func(_ context.Context, _ *dbModel.Movie, _ string, _ *cache.EmbyUserCache, gotNow time.Time) error {
			if !gotNow.After(grantedAt.Add(dbModel.EmbyRootGrantLease)) {
				t.Fatal("test did not pass a time after the grant lease")
			}
			return db.NewEmbyGrantError("not_found")
		},
		func(context.Context, *cache.EmbyUserCache) (*cache.EmbyMovieCacheData, error) {
			getCalled = true
			return &cache.EmbyMovieCacheData{
				TranscodeSessionID: "must-not-leak",
				Sources: []cache.EmbySource{{
					URL: "https://creator.example/video.m3u8?api_key=secret",
					Subtitles: []*cache.EmbySubtitleCache{{
						URL: "https://creator.example/subtitle.vtt?api_key=secret",
					}},
				}},
			}, nil
		},
	)
	if got != nil {
		t.Fatalf("cache data exposed after authorization expiry: %#v", got)
	}
	if getCalled {
		t.Fatal("cache getter ran after authorization denial")
	}
	if !errors.Is(err, db.ErrEmbyGrantDenied) {
		t.Fatalf("error = %v, want ErrEmbyGrantDenied", err)
	}
}

func TestLoadAuthorizedEmbyMovieCacheDataStopsOnInternalError(t *testing.T) {
	creatorCache := cache.NewEmbyUserCache("creator")
	internalErr := errors.New("creator cache backend failed")
	getCalled := false

	got, err := loadAuthorizedEmbyMovieCacheData(
		context.Background(),
		&dbModel.Movie{ID: "movie-id"},
		"child-id",
		"creator-id",
		time.Now,
		func(context.Context, string) (*cache.EmbyUserCache, error) {
			return creatorCache, nil
		},
		func(context.Context, *dbModel.Movie, string, *cache.EmbyUserCache, time.Time) error {
			return internalErr
		},
		func(context.Context, *cache.EmbyUserCache) (*cache.EmbyMovieCacheData, error) {
			getCalled = true
			return &cache.EmbyMovieCacheData{}, nil
		},
	)
	if got != nil {
		t.Fatalf("cache data returned on internal error: %#v", got)
	}
	if getCalled {
		t.Fatal("cache getter ran after internal authorization failure")
	}
	if !errors.Is(err, internalErr) {
		t.Fatalf("error = %v, want internal error", err)
	}
	if errors.Is(err, db.ErrEmbyGrantDenied) {
		t.Fatalf("internal error was misclassified as denial: %v", err)
	}
}

func TestWriteEmbyAccessErrorDoesNotLeakSensitiveDetails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name      string
		err       error
		status    int
		wantError string
		internal  string
	}{
		{
			name:      "denied",
			err:       db.NewEmbyGrantError("not_found"),
			status:    http.StatusForbidden,
			wantError: db.ErrEmbyGrantDenied.Error(),
			internal:  "failed to load media source",
		},
		{
			name:      "internal",
			err:       errors.New("https://creator.example/video?api_key=secret"),
			status:    http.StatusInternalServerError,
			wantError: "failed to load media source",
			internal:  "failed to load media source",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			writeEmbyAccessError(ctx, log.New().WithField("test", tt.name), tt.err, tt.internal)

			if recorder.Code != tt.status {
				t.Fatalf("status = %d, want %d", recorder.Code, tt.status)
			}
			var resp model.APIResp
			if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp.Error != tt.wantError {
				t.Fatalf("error = %q, want %q", resp.Error, tt.wantError)
			}
			for _, sensitive := range []string{"creator.example", "api_key", "secret"} {
				if strings.Contains(recorder.Body.String(), sensitive) {
					t.Fatalf("response leaked %q: %s", sensitive, recorder.Body.String())
				}
			}
		})
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
