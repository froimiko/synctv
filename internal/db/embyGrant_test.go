package db

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/synctv-org/synctv/internal/conf"
	"github.com/synctv-org/synctv/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func initEmbyGrantTestDB(t *testing.T) {
	t.Helper()
	testDB, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
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
	if err := testDB.AutoMigrate(&model.EmbyRootGrant{}); err != nil {
		t.Fatalf("migrate grant: %v", err)
	}
	restore = SwapDatabaseForTesting(testDB, conf.DatabaseTypeSqlite3)
}

func grant(parent, child string, folder bool, expiresAt time.Time) *model.EmbyRootGrant {
	return &model.EmbyRootGrant{
		MovieID: "movie", Generation: "generation", ParentItemID: parent,
		ChildItemID: child, IsFolder: folder, GrantedAt: expiresAt.Add(-time.Minute), ExpiresAt: expiresAt,
	}
}

func TestEmbyGrantErrorClassification(t *testing.T) {
	for _, category := range []string{"not_found", "not_folder"} {
		err := NewEmbyGrantError(category)
		if !errors.Is(err, ErrEmbyGrantDenied) || errors.Is(err, ErrEmbyGrantInternal) {
			t.Fatalf("category %q was not classified as denial: %v", category, err)
		}
		if EmbyGrantErrorCategory(err) != category || strings.Contains(err.Error(), category) {
			t.Fatalf("category %q was not preserved privately: %v", category, err)
		}
	}

	for _, category := range []string{
		"invalid_context", "cycle", "node_limit", "depth_limit", "database_error",
		"malformed_edge", "malformed_response", "nil_response", "invalid_generation", "unknown_category",
	} {
		err := NewEmbyGrantError(category)
		if errors.Is(err, ErrEmbyGrantDenied) || !errors.Is(err, ErrEmbyGrantInternal) {
			t.Fatalf("category %q was not classified as internal: %v", category, err)
		}
		if EmbyGrantErrorCategory(err) != category || strings.Contains(err.Error(), category) {
			t.Fatalf("category %q was not preserved privately: %v", category, err)
		}
	}
}

func TestSwapDatabaseForTestingRestoresOriginalState(t *testing.T) {
	originalDB, originalDBType := DB(), dbType
	testDB := &gorm.DB{}
	restore := SwapDatabaseForTesting(testDB, conf.DatabaseTypeSqlite3)
	if DB() != testDB || dbType != conf.DatabaseTypeSqlite3 {
		restore()
		t.Fatal("test database state was not installed")
	}
	restore()
	restore()
	if DB() != originalDB || dbType != originalDBType {
		t.Fatal("original database state was not restored")
	}
}

func TestEmbyRootGrantBatchDuplicateIsFirstWinsAndFolderRemainsTraversable(t *testing.T) {
	initEmbyGrantTestDB(t)
	now := time.Now().UTC()
	firstInBatch := grant("root", "folder", true, now.Add(time.Minute))
	laterDuplicate := grant("root", "folder", false, now.Add(2*time.Minute))
	if err := UpsertEmbyRootGrants([]*model.EmbyRootGrant{
		nil,
		firstInBatch,
		laterDuplicate,
		grant("folder", "item", false, now.Add(time.Minute)),
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	var stored model.EmbyRootGrant
	if err := db.Where(
		"movie_id = ? AND generation = ? AND parent_item_id = ? AND child_item_id = ?",
		"movie", "generation", "root", "folder",
	).First(&stored).Error; err != nil {
		t.Fatalf("load deduplicated grant: %v", err)
	}
	if !stored.IsFolder || !stored.ExpiresAt.Equal(firstInBatch.ExpiresAt) {
		t.Fatalf("input batch was not first-wins: %#v", stored)
	}
	if err := AuthorizeEmbyRootGrant("movie", "generation", "root", "item", false, now); err != nil {
		t.Fatalf("first-wins folder edge was not traversable: %v", err)
	}
	if err := AuthorizeEmbyRootGrant("movie", "generation", "root", "item", true, now); !errors.Is(err, ErrEmbyGrantDenied) {
		t.Fatalf("non-folder media authorized as folder: %v", err)
	}
}

func TestEmbyRootGrantFailsClosedForExpiryGenerationAndCycle(t *testing.T) {
	initEmbyGrantTestDB(t)
	now := time.Now().UTC()
	if err := UpsertEmbyRootGrants([]*model.EmbyRootGrant{
		grant("root", "folder", true, now.Add(time.Minute)),
		grant("folder", "root", true, now.Add(time.Minute)),
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	for name, tc := range map[string]struct {
		generation string
		at         time.Time
		want       error
		category   string
	}{
		"old generation": {generation: "old", at: now, want: ErrEmbyGrantDenied, category: "not_found"},
		"expired":        {generation: "generation", at: now.Add(2 * time.Minute), want: ErrEmbyGrantDenied, category: "not_found"},
		"cycle":          {generation: "generation", at: now, want: ErrEmbyGrantInternal, category: "cycle"},
	} {
		t.Run(name, func(t *testing.T) {
			err := AuthorizeEmbyRootGrant("movie", tc.generation, "root", "missing", false, tc.at)
			if !errors.Is(err, tc.want) || EmbyGrantErrorCategory(err) != tc.category {
				t.Fatalf("error = %v, category = %q", err, EmbyGrantErrorCategory(err))
			}
		})
	}
}

func TestEmbyRootGrantDeletion(t *testing.T) {
	initEmbyGrantTestDB(t)
	now := time.Now().UTC()
	old := grant("root", "old", false, now.Add(-time.Minute))
	boundary := grant("root", "boundary", false, now)
	current := grant("root", "current", false, now.Add(time.Minute))
	current.Generation = "current-generation"
	if err := UpsertEmbyRootGrants([]*model.EmbyRootGrant{old, boundary, current}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := DeleteExpiredEmbyRootGrants(now); err != nil {
		t.Fatalf("delete expired: %v", err)
	}
	var expiredCount int64
	if err := db.Model(&model.EmbyRootGrant{}).
		Where("child_item_id IN ?", []string{"old", "boundary"}).Count(&expiredCount).Error; err != nil || expiredCount != 0 {
		t.Fatalf("expired boundary grants = %d, error = %v", expiredCount, err)
	}
	if err := DeleteEmbyRootGrantsExceptGeneration("movie", "current-generation"); err != nil {
		t.Fatalf("delete old generation: %v", err)
	}
	if err := DeleteEmbyRootGrantsByMovieID("movie"); err != nil {
		t.Fatalf("delete movie grants: %v", err)
	}
	var count int64
	if err := db.Model(&model.EmbyRootGrant{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("remaining grants = %d, error = %v", count, err)
	}
}
