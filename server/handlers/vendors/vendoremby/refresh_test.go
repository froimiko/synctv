package vendoremby

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/synctv-org/synctv/internal/cache"
)

func TestIsEmbyUnauthorized(t *testing.T) {
	// The vendors module is a separate Go module and the gRPC boundary flattens
	// every upstream failure into a formatted string, so this classification is
	// necessarily textual. These cases pin the exact shapes that reach us.
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "expired session token",
			err:  fmt.Errorf("status code %d: %s", http.StatusUnauthorized, `{"error":"invalid token"}`),
			want: true,
		},
		{
			name: "bare invalid token body",
			err:  errors.New(`{"error":"invalid token"}`),
			want: true,
		},
		{
			name: "wrapped unauthorized",
			err:  fmt.Errorf("emby fs list error: %w", fmt.Errorf("status code %d: denied", http.StatusUnauthorized)),
			want: true,
		},
		{
			name: "not found is not unauthorized",
			err:  fmt.Errorf("status code %d: missing", http.StatusNotFound),
			want: false,
		},
		{
			name: "forbidden is not unauthorized",
			err:  fmt.Errorf("status code %d: forbidden", http.StatusForbidden),
			want: false,
		},
		{
			name: "server error is not unauthorized",
			err:  fmt.Errorf("status code %d: boom", http.StatusInternalServerError),
			want: false,
		},
		{
			name: "network error is not unauthorized",
			err:  errors.New("dial tcp: connection refused"),
			want: false,
		},
		{
			name: "nil is not unauthorized",
			err:  nil,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isEmbyUnauthorized(tt.err); got != tt.want {
				t.Fatalf("isEmbyUnauthorized(%v) = %t, want %t", tt.err, got, tt.want)
			}
		})
	}
}

func unauthorizedError() error {
	return fmt.Errorf("status code %d: %s", http.StatusUnauthorized, `{"error":"invalid token"}`)
}

func TestWithEmbyTokenRefreshSucceedsWithoutRefreshing(t *testing.T) {
	original := &cache.EmbyUserCacheData{APIKey: "first"}

	var calls int
	var refreshes int

	got, err := withEmbyTokenRefresh(
		context.Background(),
		func(context.Context) (*cache.EmbyUserCacheData, error) {
			refreshes++
			return &cache.EmbyUserCacheData{APIKey: "second"}, nil
		},
		func(_ context.Context, aucd *cache.EmbyUserCacheData) (string, error) {
			calls++
			return aucd.APIKey, nil
		},
		original,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got != "first" {
		t.Fatalf("result = %q, want first", got)
	}

	if calls != 1 {
		t.Fatalf("call count = %d, want 1", calls)
	}

	if refreshes != 0 {
		t.Fatalf("refresh count = %d, want 0", refreshes)
	}
}

func TestWithEmbyTokenRefreshRetriesOnceAfterUnauthorized(t *testing.T) {
	var calls int
	var refreshes int

	got, err := withEmbyTokenRefresh(
		context.Background(),
		func(context.Context) (*cache.EmbyUserCacheData, error) {
			refreshes++
			return &cache.EmbyUserCacheData{APIKey: "second"}, nil
		},
		func(_ context.Context, aucd *cache.EmbyUserCacheData) (string, error) {
			calls++
			if aucd.APIKey == "first" {
				return "", unauthorizedError()
			}
			return aucd.APIKey, nil
		},
		&cache.EmbyUserCacheData{APIKey: "first"},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got != "second" {
		t.Fatalf("result = %q, want second (the refreshed token was not used)", got)
	}

	if calls != 2 {
		t.Fatalf("call count = %d, want 2", calls)
	}

	if refreshes != 1 {
		t.Fatalf("refresh count = %d, want 1", refreshes)
	}
}

func TestWithEmbyTokenRefreshRetriesAtMostOnce(t *testing.T) {
	// A server that rejects even a freshly minted token must not make the
	// controller spin: exactly one retry, then give up.
	var calls int
	var refreshes int

	_, err := withEmbyTokenRefresh(
		context.Background(),
		func(context.Context) (*cache.EmbyUserCacheData, error) {
			refreshes++
			return &cache.EmbyUserCacheData{APIKey: "second"}, nil
		},
		func(context.Context, *cache.EmbyUserCacheData) (string, error) {
			calls++
			return "", unauthorizedError()
		},
		&cache.EmbyUserCacheData{APIKey: "first"},
	)
	if !isEmbyUnauthorized(err) {
		t.Fatalf("error = %v, want an unauthorized error", err)
	}

	if calls != 2 {
		t.Fatalf("call count = %d, want exactly 2", calls)
	}

	if refreshes != 1 {
		t.Fatalf("refresh count = %d, want exactly 1", refreshes)
	}
}

func TestWithEmbyTokenRefreshReturnsOriginalErrorWhenRefreshFails(t *testing.T) {
	var calls int

	_, err := withEmbyTokenRefresh(
		context.Background(),
		func(context.Context) (*cache.EmbyUserCacheData, error) {
			return nil, ErrEmbyCredentialsUnavailable
		},
		func(context.Context, *cache.EmbyUserCacheData) (string, error) {
			calls++
			return "", unauthorizedError()
		},
		&cache.EmbyUserCacheData{APIKey: "first"},
	)
	// The unauthorized error describes what the caller asked for and is more
	// actionable than the refresh failure, which is logged instead.
	if !isEmbyUnauthorized(err) {
		t.Fatalf("error = %v, want the original unauthorized error", err)
	}

	if calls != 1 {
		t.Fatalf("call count = %d, want 1", calls)
	}
}

func TestWithEmbyTokenRefreshIgnoresOtherErrors(t *testing.T) {
	sentinel := errors.New("boom")

	var calls int
	var refreshes int

	_, err := withEmbyTokenRefresh(
		context.Background(),
		func(context.Context) (*cache.EmbyUserCacheData, error) {
			refreshes++
			return &cache.EmbyUserCacheData{}, nil
		},
		func(context.Context, *cache.EmbyUserCacheData) (string, error) {
			calls++
			return "", sentinel
		},
		&cache.EmbyUserCacheData{APIKey: "first"},
	)
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want the original error", err)
	}

	if calls != 1 {
		t.Fatalf("call count = %d, want 1", calls)
	}

	if refreshes != 0 {
		t.Fatalf("refresh count = %d, want 0", refreshes)
	}
}
