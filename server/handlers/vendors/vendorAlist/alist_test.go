package vendoralist

import (
	"context"
	"testing"

	"github.com/synctv-org/synctv/internal/cache"
)

func TestDynamicMovieAlistCredentialsFromCache(t *testing.T) {
	const serverID = "creator-server"

	creatorCache := cache.NewAlistUserCache("creator")
	if _, err := creatorCache.LoadOrStoreWithDynamicFunc(
		context.Background(),
		serverID,
		func(context.Context, string, ...struct{}) (*cache.AlistUserCacheData, error) {
			return &cache.AlistUserCacheData{
				Host:     "https://creator.example",
				ServerID: serverID,
				Token:    "creator-token",
				Backend:  "creator-backend",
			}, nil
		},
	); err != nil {
		t.Fatalf("seed creator cache: %v", err)
	}

	credentials, err := dynamicMovieAlistCredentialsFromCache(
		context.Background(), creatorCache, serverID,
	)
	if err != nil {
		t.Fatalf("load dynamic movie credentials: %v", err)
	}
	if credentials.host != "https://creator.example" {
		t.Fatalf("host mismatch")
	}
	if credentials.token != "creator-token" {
		t.Fatalf("token mismatch")
	}
	if credentials.backend != "creator-backend" {
		t.Fatalf("backend mismatch")
	}
}
