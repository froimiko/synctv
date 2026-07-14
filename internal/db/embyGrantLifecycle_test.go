package db

import (
	"testing"
	"time"

	"github.com/synctv-org/synctv/internal/conf"
	"github.com/synctv-org/synctv/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func initEmbyGrantLifecycleTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	testDB, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared&_pragma=foreign_keys(1)"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	restore := func() {}
	t.Cleanup(func() {
		restore()
		sqlDB, err := testDB.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := testDB.AutoMigrate(
		&model.User{}, &model.UserProvider{}, &model.Room{}, &model.RoomSettings{}, &model.RoomMember{},
		&model.Movie{}, &model.BilibiliVendor{}, &model.AlistVendor{}, &model.EmbyVendor{}, &model.EmbyRootGrant{},
	); err != nil {
		t.Fatalf("migrate lifecycle models: %v", err)
	}
	restore = SwapDatabaseForTesting(testDB, conf.DatabaseTypeSqlite3)
	return testDB
}

func createGrantLifecycleRoom(t *testing.T, testDB *gorm.DB, userID, roomID string) {
	t.Helper()
	if err := testDB.Create(&model.User{
		ID: userID, Username: userID, HashedPassword: []byte("password"), Role: model.RoleUser,
	}).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := testDB.Create(&model.Room{
		ID: roomID, Name: roomID, CreatorID: userID, Status: model.RoomStatusActive,
	}).Error; err != nil {
		t.Fatalf("create room: %v", err)
	}
}

func lifecycleGrant(movieID string, expiresAt time.Time) *model.EmbyRootGrant {
	return &model.EmbyRootGrant{
		MovieID: movieID, Generation: "generation", ParentItemID: "root", ChildItemID: "item",
		GrantedAt: expiresAt.Add(-time.Minute), ExpiresAt: expiresAt,
	}
}

func assertLifecycleCounts(t *testing.T, testDB *gorm.DB, movieIDs []string, movies, grants int64) {
	t.Helper()
	var movieCount, grantCount int64
	if err := testDB.Unscoped().Model(&model.Movie{}).Where("id IN ?", movieIDs).Count(&movieCount).Error; err != nil {
		t.Fatalf("count movies: %v", err)
	}
	if err := testDB.Model(&model.EmbyRootGrant{}).Where("movie_id IN ?", movieIDs).Count(&grantCount).Error; err != nil {
		t.Fatalf("count grants: %v", err)
	}
	if movieCount != movies || grantCount != grants {
		t.Fatalf("movies = %d, grants = %d, want %d and %d", movieCount, grantCount, movies, grants)
	}
}

func TestSaveMovieDeletesGrantAtomically(t *testing.T) {
	testDB := initEmbyGrantLifecycleTestDB(t)
	createGrantLifecycleRoom(t, testDB, "user-save", "room-save")
	movie := &model.Movie{ID: "movie-save", RoomID: "room-save", CreatorID: "user-save", MovieBase: model.MovieBase{Name: "old"}}
	if err := testDB.Create(movie).Error; err != nil {
		t.Fatalf("create movie: %v", err)
	}
	if err := testDB.Create(lifecycleGrant(movie.ID, time.Now().UTC().Add(time.Minute))).Error; err != nil {
		t.Fatalf("create grant: %v", err)
	}
	movie.Name = "new"
	if err := SaveMovie(movie); err != nil {
		t.Fatalf("save movie: %v", err)
	}
	assertLifecycleCounts(t, testDB, []string{movie.ID}, 1, 0)
}

func TestUpdateMovieDeletesGrantAtomically(t *testing.T) {
	testDB := initEmbyGrantLifecycleTestDB(t)
	createGrantLifecycleRoom(t, testDB, "user-update", "room-update")
	movie := &model.Movie{ID: "movie-update", RoomID: "room-update", CreatorID: "user-update", MovieBase: model.MovieBase{Name: "old"}}
	if err := testDB.Create(movie).Error; err != nil {
		t.Fatalf("create movie: %v", err)
	}
	if err := testDB.Create(lifecycleGrant(movie.ID, time.Now().UTC().Add(time.Minute))).Error; err != nil {
		t.Fatalf("create grant: %v", err)
	}
	movie.Name = "new"
	if err := UpdateMovie(movie); err != nil {
		t.Fatalf("update movie: %v", err)
	}
	assertLifecycleCounts(t, testDB, []string{movie.ID}, 1, 0)
}

func TestSaveMovieGrantFailureRollsBackMovie(t *testing.T) {
	testDB := initEmbyGrantLifecycleTestDB(t)
	createGrantLifecycleRoom(t, testDB, "user-rollback", "room-rollback")
	movie := &model.Movie{ID: "movie-rollback", RoomID: "room-rollback", CreatorID: "user-rollback", MovieBase: model.MovieBase{Name: "old"}}
	if err := testDB.Create(movie).Error; err != nil {
		t.Fatalf("create movie: %v", err)
	}
	if err := testDB.Migrator().DropTable(&model.EmbyRootGrant{}); err != nil {
		t.Fatalf("drop grant table: %v", err)
	}
	movie.Name = "new"
	if err := SaveMovie(movie); err == nil {
		t.Fatal("save succeeded without grant table")
	}
	var loaded model.Movie
	if err := testDB.First(&loaded, "id = ?", movie.ID).Error; err != nil {
		t.Fatalf("reload movie: %v", err)
	}
	if loaded.Name != "old" {
		t.Fatalf("movie update was not rolled back: %q", loaded.Name)
	}
}

func TestDeleteMovieAndParentClearDeleteDescendantGrants(t *testing.T) {
	for name, deleteMovies := range map[string]func(string) error{
		"single parent": func(roomID string) error { return DeleteMovieByID(roomID, "folder") },
		"root clear":    func(roomID string) error { return DeleteMoviesByRoomIDAndParentID(roomID, "") },
	} {
		t.Run(name, func(t *testing.T) {
			testDB := initEmbyGrantLifecycleTestDB(t)
			createGrantLifecycleRoom(t, testDB, "user-closure", "room-closure")
			movies := []*model.Movie{
				{ID: "folder", RoomID: "room-closure", CreatorID: "user-closure", MovieBase: model.MovieBase{Name: "folder", IsFolder: true}},
				{ID: "child", RoomID: "room-closure", CreatorID: "user-closure", MovieBase: model.MovieBase{Name: "child", IsFolder: true, ParentID: "folder"}},
				{ID: "leaf", RoomID: "room-closure", CreatorID: "user-closure", MovieBase: model.MovieBase{Name: "leaf", ParentID: "child"}},
			}
			if err := testDB.Create(&movies).Error; err != nil {
				t.Fatalf("create movies: %v", err)
			}
			now := time.Now().UTC().Add(time.Minute)
			for _, movie := range movies {
				if err := testDB.Create(lifecycleGrant(movie.ID, now)).Error; err != nil {
					t.Fatalf("create grant: %v", err)
				}
			}
			if err := deleteMovies("room-closure"); err != nil {
				t.Fatalf("delete closure: %v", err)
			}
			assertLifecycleCounts(t, testDB, []string{"folder", "child", "leaf"}, 0, 0)
		})
	}
}

func TestRoomAndUserDeleteRemoveRoomMovieGrants(t *testing.T) {
	for name, remove := range map[string]func(string) error{
		"room": DeleteRoomByID,
		"user": DeleteUserByID,
	} {
		t.Run(name, func(t *testing.T) {
			testDB := initEmbyGrantLifecycleTestDB(t)
			userID, roomID, movieID := "user-"+name, "room-"+name, "movie-"+name
			createGrantLifecycleRoom(t, testDB, userID, roomID)
			if err := testDB.Create(&model.Movie{ID: movieID, RoomID: roomID, CreatorID: userID, MovieBase: model.MovieBase{Name: movieID}}).Error; err != nil {
				t.Fatalf("create movie: %v", err)
			}
			if err := testDB.Create(lifecycleGrant(movieID, time.Now().UTC().Add(time.Minute))).Error; err != nil {
				t.Fatalf("create grant: %v", err)
			}
			if err := remove(map[string]string{"room": roomID, "user": userID}[name]); err != nil {
				t.Fatalf("delete %s: %v", name, err)
			}
			assertLifecycleCounts(t, testDB, []string{movieID}, 0, 0)
		})
	}
}
