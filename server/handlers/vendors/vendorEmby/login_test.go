package vendoremby

import (
	"context"
	"testing"
	"time"

	"github.com/synctv-org/synctv/internal/cache"
	dbModel "github.com/synctv-org/synctv/internal/model"
	"github.com/synctv-org/vendors/api/emby"
)

func TestValidateEmbyLoginResponseRejectsIncompleteIdentity(t *testing.T) {
	tests := []struct {
		name string
		data *emby.LoginResp
	}{
		{name: "nil", data: nil},
		{name: "empty server ID", data: &emby.LoginResp{Token: "test-token", UserId: "emby-user"}},
		{name: "empty token", data: &emby.LoginResp{ServerId: "server", UserId: "emby-user"}},
		{name: "empty user ID", data: &emby.LoginResp{ServerId: "server", Token: "test-token"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateEmbyLoginResponse(tt.data); err == nil {
				t.Fatal("incomplete login response was accepted")
			}
		})
	}
	if err := validateEmbyLoginResponse(&emby.LoginResp{
		ServerId: "server", Token: "test-token", UserId: "emby-user",
	}); err != nil {
		t.Fatalf("complete login response rejected: %v", err)
	}
}

func TestEmbyUserCacheDataFromVendorUsesCanonicalBindingTimestamp(t *testing.T) {
	updatedAt := time.Date(2026, time.July, 13, 1, 2, 3, 0, time.UTC)
	data, err := embyUserCacheDataFromVendor(&dbModel.EmbyVendor{
		Host:       "https://emby.invalid",
		ServerID:   "server",
		APIKey:     "test-token",
		Backend:    "test-backend",
		EmbyUserID: "emby-user",
		UpdatedAt:  updatedAt,
	})
	if err != nil {
		t.Fatalf("convert canonical vendor: %v", err)
	}
	if data.Host != "https://emby.invalid" || data.ServerID != "server" || data.APIKey != "test-token" ||
		data.Backend != "test-backend" || data.UserID != "emby-user" {
		t.Fatal("cache identity does not match canonical row")
	}
	if !data.BindingUpdatedAt.Equal(updatedAt) {
		t.Fatalf("BindingUpdatedAt = %v, want %v", data.BindingUpdatedAt, updatedAt)
	}

	invalid := &dbModel.EmbyVendor{
		Host: "https://emby.invalid", ServerID: "server", APIKey: "test-token", EmbyUserID: "emby-user",
	}
	if _, err := embyUserCacheDataFromVendor(invalid); err == nil {
		t.Fatal("zero canonical UpdatedAt was accepted")
	}
}

func TestCachedEmbyLogoutDataCopiesBeforeCacheDelete(t *testing.T) {
	userCache := cache.NewEmbyUserCache("user")
	const serverID = "server"
	original := &cache.EmbyUserCacheData{
		Host: "https://emby.invalid", ServerID: serverID, APIKey: "test-token", UserID: "emby-user",
	}
	if _, err := userCache.StoreOrRefreshWithDynamicFunc(
		context.Background(), serverID,
		func(context.Context, string) (*cache.EmbyUserCacheData, error) { return original, nil },
	); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	copied := cachedEmbyLogoutData(userCache, serverID)
	if copied == nil || copied == original {
		t.Fatal("logout data was not copied by value")
	}
	original.APIKey = "changed-after-copy"
	if copied.APIKey != "test-token" {
		t.Fatal("copied logout data changed with cached pointer")
	}

	deleteEmbyCachedBinding(userCache, serverID)
	if _, ok := userCache.LoadCache(serverID); ok {
		t.Fatal("cache entry remained after binding deletion")
	}
	if copied.APIKey != "test-token" {
		t.Fatal("cache deletion changed copied logout data")
	}
}
