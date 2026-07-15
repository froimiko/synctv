package cache

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/synctv-org/synctv/internal/conf"
	"github.com/synctv-org/synctv/internal/db"
	"github.com/synctv-org/synctv/internal/model"
	"github.com/synctv-org/vendors/api/emby"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestEmbyGrantGenerationIsStableAndBindingScoped(t *testing.T) {
	updatedAt := time.Unix(100, 200).UTC()
	args := []string{"movie", "creator", "backend", "server", "root", "emby-user"}
	first, err := EmbyGrantGeneration(args[0], args[1], args[2], args[3], args[4], args[5], updatedAt)
	if err != nil {
		t.Fatalf("generation: %v", err)
	}
	second, err := EmbyGrantGeneration(args[0], args[1], args[2], args[3], args[4], args[5], updatedAt)
	if err != nil || first != second {
		t.Fatal("generation is not stable")
	}
	changed, err := EmbyGrantGeneration(args[0], args[1], args[2], args[3], "other-root", args[5], updatedAt)
	if err != nil || changed == first {
		t.Fatal("root change did not rotate generation")
	}
	for _, secret := range []string{"host", "token", "api-key"} {
		if strings.Contains(first, secret) {
			t.Fatal("generation exposed secret material")
		}
	}
}

func TestEmbyGrantGenerationRejectsIncompleteContext(t *testing.T) {
	for name, tc := range map[string]struct {
		userID    string
		updatedAt time.Time
	}{
		"missing user":              {updatedAt: time.Unix(100, 0).UTC()},
		"missing binding timestamp": {userID: "user"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := EmbyGrantGeneration(
				"movie", "creator", "backend", "server", "root", tc.userID, tc.updatedAt,
			); err == nil {
				t.Fatal("expected incomplete generation context rejection")
			}
		})
	}
}

func TestNewEmbyRootGrantsRejectsMalformedItems(t *testing.T) {
	for _, items := range [][]*emby.Item{{nil}, {{}}} {
		if _, err := NewEmbyRootGrants("movie", "generation", "parent", items, time.Now()); err == nil {
			t.Fatal("expected malformed item rejection")
		}
	}
}

func TestNewEmbyRootGrantsUsesOnlyReturnedUniqueItems(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	grants, err := NewEmbyRootGrants("movie", "generation", "parent", []*emby.Item{
		{Id: "folder", IsFolder: true},
		{Id: "folder", IsFolder: false},
		{Id: "movie", IsFolder: false},
	}, now)
	if err != nil {
		t.Fatalf("build grants: %v", err)
	}
	if len(grants) != 2 {
		t.Fatalf("grant count = %d, want 2", len(grants))
	}
	if grants[0].ChildItemID != "folder" || !grants[0].IsFolder {
		t.Fatalf("first grant = %#v", grants[0])
	}
	if !grants[0].ExpiresAt.Equal(now.Add(model.EmbyRootGrantLease)) {
		t.Fatalf("expiry = %v", grants[0].ExpiresAt)
	}
}

func testEmbyMovieAndBinding() (*model.Movie, *EmbyUserCache, time.Time) {
	bindingUpdatedAt := time.Unix(100, 200).UTC()
	movie := &model.Movie{
		ID:        "movie",
		CreatorID: "creator",
		MovieBase: model.MovieBase{
			IsFolder: true,
			VendorInfo: model.VendorInfo{
				Vendor: model.VendorEmby,
				Emby:   &model.EmbyStreamingInfo{Path: "server/root"},
			},
		},
	}
	creatorCache := NewEmbyUserCache("creator")
	_, _ = creatorCache.LoadOrStoreWithDynamicFunc(
		context.Background(),
		"server",
		func(context.Context, string) (*EmbyUserCacheData, error) {
			return &EmbyUserCacheData{
				Host:             "https://emby.example",
				ServerID:         "server",
				APIKey:           "secret",
				UserID:           "emby-user",
				Backend:          "backend",
				BindingUpdatedAt: bindingUpdatedAt,
			}, nil
		},
	)
	return movie, creatorCache, bindingUpdatedAt
}

func TestValidateEmbyMovieGrantUsesCreatorCacheBindingGeneration(t *testing.T) {
	movie, creatorCache, bindingUpdatedAt := testEmbyMovieAndBinding()
	now := time.Unix(200, 0).UTC()
	wantGeneration, err := EmbyGrantGeneration(
		movie.ID, movie.CreatorID, "backend", "server", "root", "emby-user", bindingUpdatedAt,
	)
	if err != nil {
		t.Fatalf("generation: %v", err)
	}
	called := false
	err = validateEmbyMovieGrant(
		context.Background(), movie, "episode", creatorCache, now,
		func(movieID, generation, rootItemID, requestedItemID string, requireFolder bool, at time.Time) error {
			called = true
			if movieID != movie.ID || generation != wantGeneration || rootItemID != "root" || requestedItemID != "episode" {
				t.Fatal("unexpected grant context")
			}
			if requireFolder || !at.Equal(now) {
				t.Fatal("unexpected playback grant requirements")
			}
			return nil
		},
	)
	if err != nil {
		t.Fatalf("validate grant: %v", err)
	}
	if !called {
		t.Fatal("grant validator was not called")
	}
}

func TestValidateEmbyMovieGrantRejectsInvalidBindingContext(t *testing.T) {
	movie, creatorCache, _ := testEmbyMovieAndBinding()
	for name, mutate := range map[string]func(*EmbyUserCacheData){
		"missing user": func(binding *EmbyUserCacheData) { binding.UserID = "" },
		"missing binding timestamp": func(binding *EmbyUserCacheData) {
			binding.BindingUpdatedAt = time.Time{}
		},
		"server mismatch": func(binding *EmbyUserCacheData) { binding.ServerID = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			binding, err := creatorCache.LoadOrStore(context.Background(), "server")
			if err != nil {
				t.Fatalf("load binding: %v", err)
			}
			copyBinding := *binding
			mutate(&copyBinding)
			cache := NewEmbyUserCache("creator")
			_, _ = cache.LoadOrStoreWithDynamicFunc(
				context.Background(), "server",
				func(context.Context, string) (*EmbyUserCacheData, error) { return &copyBinding, nil },
			)
			called := false
			err = validateEmbyMovieGrant(
				context.Background(), movie, "episode", cache, time.Unix(200, 0).UTC(),
				func(string, string, string, string, bool, time.Time) error {
					called = true
					return nil
				},
			)
			if err == nil || called {
				t.Fatal("invalid binding was not rejected before grant lookup")
			}
			if name != "server mismatch" {
				if errors.Is(err, db.ErrEmbyGrantDenied) || !errors.Is(err, db.ErrEmbyGrantInternal) {
					t.Fatalf("generation failure error = %v, want internal grant failure", err)
				}
				if db.EmbyGrantErrorCategory(err) != "invalid_generation" {
					t.Fatalf("generation failure category = %q", db.EmbyGrantErrorCategory(err))
				}
			}
			if name == "server mismatch" && (errors.Is(err, db.ErrEmbyGrantDenied) || errors.Is(err, db.ErrEmbyGrantInternal)) {
				t.Fatal("binding cache corruption was misclassified as a grant error")
			}
		})
	}
}

func initEmbyGrantCacheTestDB(t *testing.T) {
	t.Helper()
	testDB, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	previousConf := conf.Conf
	restore := func() {}
	t.Cleanup(func() {
		conf.Conf = previousConf
		restore()
		sqlDB, err := testDB.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := testDB.AutoMigrate(&model.EmbyRootGrant{}); err != nil {
		t.Fatalf("migrate grant: %v", err)
	}
	conf.Conf = conf.DefaultConfig()
	conf.Conf.Database.Type = conf.DatabaseTypeSqlite3
	restore = db.SwapDatabaseForTesting(testDB, conf.DatabaseTypeSqlite3)
}

func insertEmbyMovieGrant(t *testing.T, movie *model.Movie, bindingUpdatedAt time.Time, grantedAt time.Time) string {
	t.Helper()
	generation, err := EmbyGrantGeneration(
		movie.ID, movie.CreatorID, "backend", "server", "root", "emby-user", bindingUpdatedAt,
	)
	if err != nil {
		t.Fatalf("generation: %v", err)
	}
	if err := db.UpsertEmbyRootGrants([]*model.EmbyRootGrant{{
		MovieID:      movie.ID,
		Generation:   generation,
		ParentItemID: "root",
		ChildItemID:  "episode",
		IsFolder:     false,
		GrantedAt:    grantedAt,
		ExpiresAt:    grantedAt.Add(model.EmbyRootGrantLease),
	}}); err != nil {
		t.Fatalf("insert grant: %v", err)
	}
	return generation
}

type playbackInfoEmbyClient struct {
	emby.UnimplementedEmbyServer
	calls int
	req   *emby.PlaybackInfoReq
}

var _ emby.EmbyHTTPServer = (*playbackInfoEmbyClient)(nil)

func (c *playbackInfoEmbyClient) PlaybackInfo(
	_ context.Context,
	req *emby.PlaybackInfoReq,
) (*emby.PlaybackInfoResp, error) {
	c.calls++
	c.req = req
	return &emby.PlaybackInfoResp{
		PlaySessionID: "session",
		MediaSourceInfo: []*emby.MediaSourceInfo{{
			Id:        "source",
			Container: "mp4",
		}},
	}, nil
}

func newEmbyMovieCacheInitForTest(
	movie *model.Movie,
	now time.Time,
	client *playbackInfoEmbyClient,
	loadCalls *int,
) func(context.Context, *EmbyUserCache) (*EmbyMovieCacheData, error) {
	return newEmbyMovieCacheInitFunc(
		movie,
		"episode",
		func() time.Time { return now },
		ValidateEmbyNavigationGrant,
		func(string) emby.EmbyHTTPServer {
			*loadCalls = *loadCalls + 1
			return client
		},
	)
}

func assertPlaybackInfoNotCalled(t *testing.T, loadCalls int, client *playbackInfoEmbyClient) {
	t.Helper()
	if loadCalls != 0 || client.calls != 0 {
		t.Fatalf("client loads = %d, PlaybackInfo calls = %d", loadCalls, client.calls)
	}
}

func TestEmbyMovieCacheInitGrantFailureDoesNotCallPlaybackInfo(t *testing.T) {
	for name, setup := range map[string]func(*testing.T, *model.Movie, time.Time, time.Time){
		"no grant": func(*testing.T, *model.Movie, time.Time, time.Time) {},
		"expired": func(t *testing.T, movie *model.Movie, bindingUpdatedAt, now time.Time) {
			insertEmbyMovieGrant(t, movie, bindingUpdatedAt, now.Add(-model.EmbyRootGrantLease-time.Second))
		},
		"old generation": func(t *testing.T, movie *model.Movie, _ time.Time, now time.Time) {
			oldBindingUpdatedAt := time.Unix(50, 0).UTC()
			insertEmbyMovieGrant(t, movie, oldBindingUpdatedAt, now)
		},
	} {
		t.Run(name, func(t *testing.T) {
			initEmbyGrantCacheTestDB(t)
			movie, creatorCache, bindingUpdatedAt := testEmbyMovieAndBinding()
			now := time.Unix(200, 0).UTC()
			setup(t, movie, bindingUpdatedAt, now)
			client := &playbackInfoEmbyClient{}
			loadCalls := 0
			initCache := newEmbyMovieCacheInitForTest(movie, now, client, &loadCalls)
			_, err := initCache(context.Background(), creatorCache)
			if !errors.Is(err, db.ErrEmbyGrantDenied) {
				t.Fatalf("error = %v, want grant denial", err)
			}
			assertPlaybackInfoNotCalled(t, loadCalls, client)
		})
	}
}

func TestEmbyMovieCacheInitCallsPlaybackInfoForLegalNonFolderGrant(t *testing.T) {
	initEmbyGrantCacheTestDB(t)
	movie, creatorCache, bindingUpdatedAt := testEmbyMovieAndBinding()
	now := time.Unix(200, 0).UTC()
	insertEmbyMovieGrant(t, movie, bindingUpdatedAt, now)
	client := &playbackInfoEmbyClient{}
	loadCalls := 0
	initCache := newEmbyMovieCacheInitForTest(movie, now, client, &loadCalls)
	data, err := initCache(context.Background(), creatorCache)
	if err != nil {
		t.Fatalf("init movie cache: %v", err)
	}
	if loadCalls != 1 || client.calls != 1 {
		t.Fatalf("client loads = %d, PlaybackInfo calls = %d", loadCalls, client.calls)
	}
	if data == nil || data.TranscodeSessionID != "session" {
		t.Fatalf("cache data = %#v", data)
	}
	if client.req == nil || client.req.GetItemId() != "episode" || client.req.GetUserId() != "emby-user" {
		t.Fatal("PlaybackInfo request used wrong binding or item")
	}
}
