package db

import (
	"testing"
	"time"

	"github.com/synctv-org/synctv/internal/conf"
	"github.com/synctv-org/synctv/internal/model"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func initEmbyVendorTestDB(t *testing.T) *gorm.DB {
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
	if err := testDB.AutoMigrate(&model.EmbyVendor{}); err != nil {
		t.Fatalf("migrate emby vendor: %v", err)
	}
	restore = SwapDatabaseForTesting(testDB, conf.DatabaseTypeSqlite3)
	return testDB
}

func TestCreateOrSaveEmbyVendorReturnsCanonicalPersistedRow(t *testing.T) {
	initEmbyVendorTestDB(t)
	input := &model.EmbyVendor{
		UserID:     "user-a",
		ServerID:   "server-a",
		Host:       "https://emby.invalid",
		APIKey:     "test-token-a",
		Backend:    "test-backend",
		EmbyUserID: "emby-user-a",
	}

	persisted, err := CreateOrSaveEmbyVendor(input)
	if err != nil {
		t.Fatalf("create emby vendor: %v", err)
	}
	if persisted == input {
		t.Fatal("returned input pointer instead of canonical persisted row")
	}
	if persisted.UpdatedAt.IsZero() {
		t.Fatal("canonical UpdatedAt is zero")
	}
	if persisted.Host != input.Host || persisted.APIKey != input.APIKey || persisted.EmbyUserID != input.EmbyUserID {
		t.Fatal("canonical identity does not match persisted input")
	}

	loaded, err := GetEmbyVendor(input.UserID, input.ServerID)
	if err != nil {
		t.Fatalf("load emby vendor: %v", err)
	}
	if !persisted.UpdatedAt.Equal(loaded.UpdatedAt) {
		t.Fatalf("returned UpdatedAt %v does not match database %v", persisted.UpdatedAt, loaded.UpdatedAt)
	}
	if persisted.Host != loaded.Host || persisted.APIKey != loaded.APIKey || persisted.EmbyUserID != loaded.EmbyUserID {
		t.Fatal("returned canonical row does not match database row")
	}
}

func TestCreateOrSaveEmbyVendorRebindRotatesCanonicalUpdatedAt(t *testing.T) {
	testDB := initEmbyVendorTestDB(t)
	first, err := CreateOrSaveEmbyVendor(&model.EmbyVendor{
		UserID:     "user-b",
		ServerID:   "server-b",
		Host:       "https://first.invalid",
		APIKey:     "test-token-b1",
		EmbyUserID: "emby-user-b1",
	})
	if err != nil {
		t.Fatalf("create initial binding: %v", err)
	}

	oldUpdatedAt := time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
	if err := testDB.Model(&model.EmbyVendor{}).
		Where("user_id = ? AND server_id = ?", first.UserID, first.ServerID).
		UpdateColumn("updated_at", oldUpdatedAt).Error; err != nil {
		t.Fatalf("seed old UpdatedAt: %v", err)
	}

	rebound, err := CreateOrSaveEmbyVendor(&model.EmbyVendor{
		UserID:     first.UserID,
		ServerID:   first.ServerID,
		Host:       "https://second.invalid",
		APIKey:     "test-token-b2",
		Backend:    "rebound-backend",
		EmbyUserID: "emby-user-b2",
	})
	if err != nil {
		t.Fatalf("rebind emby vendor: %v", err)
	}
	if rebound.UpdatedAt.IsZero() || !rebound.UpdatedAt.After(oldUpdatedAt) {
		t.Fatalf("rebind UpdatedAt did not rotate: %v", rebound.UpdatedAt)
	}
	if rebound.Host != "https://second.invalid" || rebound.APIKey != "test-token-b2" || rebound.EmbyUserID != "emby-user-b2" {
		t.Fatal("rebind did not return the new canonical identity")
	}

	loaded, err := GetEmbyVendor(first.UserID, first.ServerID)
	if err != nil {
		t.Fatalf("load rebound vendor: %v", err)
	}
	if !rebound.UpdatedAt.Equal(loaded.UpdatedAt) {
		t.Fatalf("rebind UpdatedAt %v does not match database %v", rebound.UpdatedAt, loaded.UpdatedAt)
	}
}

func TestCreateOrSaveEmbyVendorRejectsIncompleteIdentityWithoutWriting(t *testing.T) {
	initEmbyVendorTestDB(t)
	valid := model.EmbyVendor{
		UserID:     "user-c",
		ServerID:   "server-c",
		Host:       "https://emby.invalid",
		APIKey:     "test-token-c",
		EmbyUserID: "emby-user-c",
	}
	tests := []struct {
		name   string
		vendor *model.EmbyVendor
	}{
		{name: "nil", vendor: nil},
		{name: "empty host", vendor: func() *model.EmbyVendor { v := valid; v.Host = ""; return &v }()},
		{name: "empty API key", vendor: func() *model.EmbyVendor { v := valid; v.APIKey = ""; return &v }()},
		{name: "empty Emby user ID", vendor: func() *model.EmbyVendor { v := valid; v.EmbyUserID = ""; return &v }()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			persisted, err := CreateOrSaveEmbyVendor(tt.vendor)
			if err == nil || persisted != nil {
				t.Fatalf("result = %#v, error = %v; want rejection", persisted, err)
			}
			var count int64
			if err := DB().Model(&model.EmbyVendor{}).Count(&count).Error; err != nil {
				t.Fatalf("count vendors: %v", err)
			}
			if count != 0 {
				t.Fatalf("invalid input wrote %d rows", count)
			}
		})
	}
}
