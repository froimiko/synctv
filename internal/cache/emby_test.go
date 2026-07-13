package cache

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/synctv-org/vendors/api/emby"
)

const (
	testEmbyHost   = "https://emby.example"
	testEmbyToken  = "secret"
	testEmbyUserID = "owner-emby-user"
)

type fakeEmbyItemGetter struct {
	item     *emby.Item
	err      error
	requests []*emby.GetItemReq
}

func (f *fakeEmbyItemGetter) GetItem(
	_ context.Context,
	req *emby.GetItemReq,
) (*emby.Item, error) {
	f.requests = append(f.requests, &emby.GetItemReq{
		Host:       req.GetHost(),
		Token:      req.GetToken(),
		ItemId:     req.GetItemId(),
		UserId:     req.GetUserId(),
		RootItemId: req.GetRootItemId(),
	})
	if f.err != nil {
		return nil, f.err
	}
	return f.item, nil
}

func assertEmbyReachabilityRequest(
	t *testing.T,
	requests []*emby.GetItemReq,
	rootItemID, requestedItemID string,
) {
	t.Helper()
	if len(requests) != 1 {
		t.Fatalf("GetItem calls = %d, want 1", len(requests))
	}
	req := requests[0]
	if got := req.GetHost(); got != testEmbyHost {
		t.Errorf("host = %q, want %q", got, testEmbyHost)
	}
	if got := req.GetToken(); got != testEmbyToken {
		t.Errorf("token mismatch")
	}
	if got := req.GetUserId(); got != testEmbyUserID {
		t.Errorf("user ID = %q, want %q", got, testEmbyUserID)
	}
	if got := req.GetRootItemId(); got != rootItemID {
		t.Errorf("root item ID = %q, want %q", got, rootItemID)
	}
	if got := req.GetItemId(); got != requestedItemID {
		t.Errorf("item ID = %q, want %q", got, requestedItemID)
	}
}

func TestValidateEmbyItemInRoot(t *testing.T) {
	tests := []struct {
		name        string
		rootItemID  string
		requestedID string
		item        *emby.Item
		getItemErr  error
		wantErr     bool
		wantCalls   int
	}{
		{name: "root itself", rootItemID: "root", requestedID: "root"},
		{name: "empty root", requestedID: "child", wantErr: true},
		{name: "empty request", rootItemID: "root", wantErr: true},
		{
			name:        "virtual root deep descendant proof",
			rootItemID:  "root",
			requestedID: "episode",
			item:        &emby.Item{Id: "episode", ParentId: "root"},
			wantCalls:   1,
		},
		{
			name:        "wrong root proof",
			rootItemID:  "root",
			requestedID: "episode",
			item:        &emby.Item{Id: "episode", ParentId: "other-root"},
			wantErr:     true,
			wantCalls:   1,
		},
		{
			name:        "empty root proof",
			rootItemID:  "root",
			requestedID: "episode",
			item:        &emby.Item{Id: "episode"},
			wantErr:     true,
			wantCalls:   1,
		},
		{
			name:        "returned ID mismatch",
			rootItemID:  "root",
			requestedID: "episode",
			item:        &emby.Item{Id: "different", ParentId: "root"},
			wantErr:     true,
			wantCalls:   1,
		},
		{
			name:        "returned empty ID",
			rootItemID:  "root",
			requestedID: "episode",
			item:        &emby.Item{ParentId: "root"},
			wantErr:     true,
			wantCalls:   1,
		},
		{
			name:        "upstream error",
			rootItemID:  "root",
			requestedID: "episode",
			getItemErr:  errors.New("upstream failure"),
			wantErr:     true,
			wantCalls:   1,
		},
		{
			name:        "nil item",
			rootItemID:  "root",
			requestedID: "episode",
			wantErr:     true,
			wantCalls:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cli := &fakeEmbyItemGetter{item: tt.item, err: tt.getItemErr}
			err := ValidateEmbyItemInRoot(
				context.Background(), cli, testEmbyHost, testEmbyToken, testEmbyUserID,
				tt.rootItemID, tt.requestedID,
			)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validation error state mismatch: error = %v, wantErr = %v", err, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(err, errEmbyItemOutsideRoot) {
				t.Fatalf("error = %v, want shared-root rejection", err)
			}
			if len(cli.requests) != tt.wantCalls {
				t.Fatalf("GetItem calls = %d, want %d", len(cli.requests), tt.wantCalls)
			}
			if tt.wantCalls == 1 {
				assertEmbyReachabilityRequest(t, cli.requests, tt.rootItemID, tt.requestedID)
			}
			if err != nil {
				for _, sensitive := range []string{testEmbyHost, testEmbyToken} {
					if strings.Contains(err.Error(), sensitive) {
						t.Fatalf("validation error leaked credentials")
					}
				}
			}
		})
	}
}

func TestValidateEmbyItemInRootRejectsMissingRequiredContext(t *testing.T) {
	tests := []struct {
		name        string
		cli         embyItemGetter
		userID      string
		rootItemID  string
		requestedID string
	}{
		{name: "nil client", userID: testEmbyUserID, rootItemID: "root", requestedID: "child"},
		{name: "empty user ID", cli: &fakeEmbyItemGetter{}, rootItemID: "root", requestedID: "child"},
		{name: "empty user ID rejects root itself", cli: &fakeEmbyItemGetter{}, rootItemID: "root", requestedID: "root"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateEmbyItemInRoot(
				context.Background(), tt.cli, testEmbyHost, testEmbyToken, tt.userID,
				tt.rootItemID, tt.requestedID,
			)
			if !errors.Is(err, errEmbyItemOutsideRoot) {
				t.Fatalf("error = %v, want shared-root rejection", err)
			}
			if cli, ok := tt.cli.(*fakeEmbyItemGetter); ok && len(cli.requests) != 0 {
				t.Fatalf("GetItem calls = %d, want 0", len(cli.requests))
			}
		})
	}
}

func TestProcessEmbySubtitlesBuildsIndependentAuthenticatedURLs(t *testing.T) {
	const (
		itemID       = "item-id"
		mediaSourceID = "media-source-id"
		apiKey       = "required-api-key"
	)

	baseURL, err := url.Parse("https://emby.example/base?video=value")
	if err != nil {
		t.Fatalf("parse base URL: %v", err)
	}
	originalBaseURL := baseURL.String()

	mediaSource := &emby.MediaSourceInfo{
		Id: mediaSourceID,
		MediaStreamInfo: []*emby.MediaStreamInfo{
			{Type: "Subtitle", Index: 2, DisplayTitle: "English"},
			{Type: "Audio", Index: 3},
			{Type: "Subtitle", Index: 7, DisplayTitle: "Japanese"},
		},
	}

	subtitles := processEmbySubtitles(mediaSource, itemID, apiKey, baseURL)
	if len(subtitles) != 2 {
		t.Fatalf("subtitle count = %d, want 2", len(subtitles))
	}
	if subtitles[0].URL == subtitles[1].URL {
		t.Fatalf("subtitle URLs are not independent: both are %q", subtitles[0].URL)
	}

	if baseURL.String() != originalBaseURL {
		t.Fatalf("base URL was mutated: got %q, want %q", baseURL.String(), originalBaseURL)
	}

	wantPaths := []string{
		"/emby/Videos/item-id/media-source-id/Subtitles/2/Stream.srt",
		"/emby/Videos/item-id/media-source-id/Subtitles/7/Stream.srt",
	}
	for i, subtitle := range subtitles {
		subtitleURL, err := url.Parse(subtitle.URL)
		if err != nil {
			t.Fatalf("parse subtitle %d URL: %v", i, err)
		}
		if subtitleURL.Path != wantPaths[i] {
			t.Errorf("subtitle %d path = %q, want %q", i, subtitleURL.Path, wantPaths[i])
		}
		if got := subtitleURL.Query().Get("api_key"); got != apiKey {
			t.Errorf("subtitle %d api_key = %q, want %q", i, got, apiKey)
		}
		if got := subtitleURL.Query().Get("video"); got != "" {
			t.Errorf("subtitle %d inherited video query = %q, want empty", i, got)
		}
	}

	secondMediaSource := &emby.MediaSourceInfo{
		Id: "second-media-source",
		MediaStreamInfo: []*emby.MediaStreamInfo{
			{Type: "Subtitle", Index: 9, DisplayTitle: "French"},
		},
	}
	secondSubtitles := processEmbySubtitles(secondMediaSource, "second-item", "second-api-key", baseURL)
	if len(secondSubtitles) != 1 {
		t.Fatalf("second subtitle count = %d, want 1", len(secondSubtitles))
	}
	if subtitles[0].URL != "https://emby.example/emby/Videos/item-id/media-source-id/Subtitles/2/Stream.srt?api_key=required-api-key" {
		t.Fatalf("first call result changed after second call: %q", subtitles[0].URL)
	}
	if got := secondSubtitles[0].URL; got != "https://emby.example/emby/Videos/second-item/second-media-source/Subtitles/9/Stream.srt?api_key=second-api-key" {
		t.Fatalf("second call URL = %q", got)
	}
	if baseURL.String() != originalBaseURL {
		t.Fatalf("base URL was mutated after consecutive calls: got %q, want %q", baseURL.String(), originalBaseURL)
	}
}

func TestNewEmbySubtitleCacheInitFuncFetchesWithAPIKey(t *testing.T) {
	const (
		apiKey = "required-api-key"
		body   = "1\n00:00:00,000 --> 00:00:01,000\nsubtitle body\n"
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/emby/Videos/item/media-source/Subtitles/4/Stream.srt" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("api_key") != apiKey {
			http.Error(w, "missing or invalid api_key", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	baseURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	mediaSource := &emby.MediaSourceInfo{
		Id: "media-source",
		MediaStreamInfo: []*emby.MediaStreamInfo{
			{Type: "Subtitle", Index: 4},
		},
	}

	subtitles := processEmbySubtitles(mediaSource, "item", apiKey, baseURL)
	if len(subtitles) != 1 {
		t.Fatalf("subtitle count = %d, want 1", len(subtitles))
	}

	got, err := newEmbySubtitleCacheInitFunc(subtitles[0].URL)(context.Background())
	if err != nil {
		t.Fatalf("fetch subtitle: %v", err)
	}
	if string(got) != body {
		t.Fatalf("subtitle body = %q, want %q", string(got), body)
	}
}

func TestNewEmbySubtitleCacheInitFuncFailureDoesNotLeakCredentials(t *testing.T) {
	const token = "fake-secret-token"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream body contains "+token, http.StatusBadGateway)
	}))
	serverURL := server.URL
	server.Close()

	_, err := newEmbySubtitleCacheInitFunc(serverURL + "/subtitle?api_key=" + token)(context.Background())
	if err == nil {
		t.Fatal("expected upstream request failure")
	}

	errorText := err.Error()
	for _, sensitive := range []string{"api_key", token, serverURL} {
		if strings.Contains(errorText, sensitive) {
			t.Fatalf("error leaked %q: %q", sensitive, errorText)
		}
	}
}

func TestNewEmbySubtitleCacheInitFuncBadStatusDoesNotLeakCredentials(t *testing.T) {
	const token = "fake-secret-token"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream body contains "+token, http.StatusBadGateway)
	}))
	defer server.Close()

	_, err := newEmbySubtitleCacheInitFunc(server.URL + "/subtitle?api_key=" + token)(context.Background())
	if err == nil {
		t.Fatal("expected non-200 status error")
	}

	errorText := err.Error()
	if !strings.Contains(errorText, "502") {
		t.Fatalf("error = %q, want status code", errorText)
	}
	for _, sensitive := range []string{"api_key", token, server.URL} {
		if strings.Contains(errorText, sensitive) {
			t.Fatalf("error leaked %q: %q", sensitive, errorText)
		}
	}
}
