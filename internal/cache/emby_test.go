package cache

import (
	"context"
	"errors"
	"fmt"
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
	items    map[string]*emby.Item
	err      error
	requests []*emby.GetItemReq
}

func (f *fakeEmbyItemGetter) GetItem(
	_ context.Context,
	req *emby.GetItemReq,
) (*emby.Item, error) {
	f.requests = append(f.requests, &emby.GetItemReq{
		Host:   req.GetHost(),
		Token:  req.GetToken(),
		ItemId: req.GetItemId(),
		UserId: req.GetUserId(),
	})
	if f.err != nil {
		return nil, f.err
	}
	return f.items[req.GetItemId()], nil
}

func assertEmbyItemRequests(t *testing.T, requests []*emby.GetItemReq, wantItemIDs []string) {
	t.Helper()

	if len(requests) != len(wantItemIDs) {
		t.Fatalf("GetItem calls = %d, want %d", len(requests), len(wantItemIDs))
	}
	for i, req := range requests {
		if got := req.GetHost(); got != testEmbyHost {
			t.Errorf("request %d host = %q, want %q", i, got, testEmbyHost)
		}
		if got := req.GetToken(); got != testEmbyToken {
			t.Errorf("request %d token mismatch", i)
		}
		if got := req.GetUserId(); got != testEmbyUserID {
			t.Errorf("request %d user ID = %q, want %q", i, got, testEmbyUserID)
		}
		if got := req.GetItemId(); got != wantItemIDs[i] {
			t.Errorf("request %d item ID = %q, want %q", i, got, wantItemIDs[i])
		}
	}
}

func TestValidateEmbyItemInRoot(t *testing.T) {
	tests := []struct {
		name        string
		rootItemID  string
		requestedID string
		items       map[string]*emby.Item
		getItemErr  error
		wantErr     bool
		wantItemIDs []string
	}{
		{
			name:        "root itself",
			rootItemID:  "root",
			requestedID: "root",
		},
		{
			name:        "empty root",
			requestedID: "child",
			wantErr:     true,
		},
		{
			name:       "empty request",
			rootItemID: "root",
			wantErr:    true,
		},
		{
			name:        "direct descendant",
			rootItemID:  "root",
			requestedID: "child",
			items: map[string]*emby.Item{
				"child": {Id: "child", ParentId: "root"},
			},
			wantItemIDs: []string{"child"},
		},
		{
			name:        "third level playable media descendant",
			rootItemID:  "root",
			requestedID: "episode",
			items: map[string]*emby.Item{
				"episode": {Id: "episode", ParentId: "season"},
				"season":  {Id: "season", ParentId: "series"},
				"series":  {Id: "series", ParentId: "root"},
			},
			wantItemIDs: []string{"episode", "season", "series"},
		},
		{
			name:        "unrelated root",
			rootItemID:  "root",
			requestedID: "child",
			items: map[string]*emby.Item{
				"child": {Id: "child", ParentId: "other"},
				"other": {Id: "other"},
			},
			wantErr:     true,
			wantItemIDs: []string{"child", "other"},
		},
		{
			name:        "empty parent",
			rootItemID:  "root",
			requestedID: "child",
			items: map[string]*emby.Item{
				"child": {Id: "child"},
			},
			wantErr:     true,
			wantItemIDs: []string{"child"},
		},
		{
			name:        "cycle",
			rootItemID:  "root",
			requestedID: "child",
			items: map[string]*emby.Item{
				"child":  {Id: "child", ParentId: "parent"},
				"parent": {Id: "parent", ParentId: "child"},
			},
			wantErr:     true,
			wantItemIDs: []string{"child", "parent"},
		},
		{
			name:        "self-parent cycle",
			rootItemID:  "root",
			requestedID: "child",
			items: map[string]*emby.Item{
				"child": {Id: "child", ParentId: "child"},
			},
			wantErr:     true,
			wantItemIDs: []string{"child"},
		},
		{
			name:        "returned ID mismatch",
			rootItemID:  "root",
			requestedID: "child",
			items: map[string]*emby.Item{
				"child": {Id: "different", ParentId: "root"},
			},
			wantErr:     true,
			wantItemIDs: []string{"child"},
		},
		{
			name:        "returned empty ID",
			rootItemID:  "root",
			requestedID: "child",
			items: map[string]*emby.Item{
				"child": {ParentId: "root"},
			},
			wantErr:     true,
			wantItemIDs: []string{"child"},
		},
		{
			name:        "GetItem error",
			rootItemID:  "root",
			requestedID: "child",
			getItemErr:  errors.New("upstream failure"),
			wantErr:     true,
			wantItemIDs: []string{"child"},
		},
		{
			name:        "nil item",
			rootItemID:  "root",
			requestedID: "missing",
			items:       map[string]*emby.Item{},
			wantErr:     true,
			wantItemIDs: []string{"missing"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cli := &fakeEmbyItemGetter{items: tt.items, err: tt.getItemErr}
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
			assertEmbyItemRequests(t, cli.requests, tt.wantItemIDs)
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
	t.Run("nil client", func(t *testing.T) {
		err := ValidateEmbyItemInRoot(
			context.Background(), nil, testEmbyHost, testEmbyToken, testEmbyUserID,
			"root", "child",
		)
		if !errors.Is(err, errEmbyItemOutsideRoot) {
			t.Fatalf("error = %v, want shared-root rejection", err)
		}
	})

	t.Run("empty user ID", func(t *testing.T) {
		cli := &fakeEmbyItemGetter{}
		err := ValidateEmbyItemInRoot(
			context.Background(), cli, testEmbyHost, testEmbyToken, "",
			"root", "child",
		)
		if !errors.Is(err, errEmbyItemOutsideRoot) {
			t.Fatalf("error = %v, want shared-root rejection", err)
		}
		if len(cli.requests) != 0 {
			t.Fatalf("GetItem calls = %d, want 0", len(cli.requests))
		}
	})

	t.Run("empty user ID rejects root itself", func(t *testing.T) {
		cli := &fakeEmbyItemGetter{}
		err := ValidateEmbyItemInRoot(
			context.Background(), cli, testEmbyHost, testEmbyToken, "",
			"root", "root",
		)
		if !errors.Is(err, errEmbyItemOutsideRoot) {
			t.Fatalf("error = %v, want shared-root rejection", err)
		}
		if len(cli.requests) != 0 {
			t.Fatalf("GetItem calls = %d, want 0", len(cli.requests))
		}
	})
}

func TestValidateEmbyItemInRootEnforcesMaximumDepth(t *testing.T) {
	t.Run("root reached on maximum query", func(t *testing.T) {
		items := make(map[string]*emby.Item, maxEmbyParentDepth)
		wantItemIDs := make([]string, maxEmbyParentDepth)
		for depth := 0; depth < maxEmbyParentDepth; depth++ {
			itemID := fmt.Sprintf("node-%03d", depth)
			parentID := fmt.Sprintf("node-%03d", depth+1)
			if depth == maxEmbyParentDepth-1 {
				parentID = "root"
			}
			items[itemID] = &emby.Item{Id: itemID, ParentId: parentID}
			wantItemIDs[depth] = itemID
		}

		cli := &fakeEmbyItemGetter{items: items}
		err := ValidateEmbyItemInRoot(
			context.Background(), cli, testEmbyHost, testEmbyToken, testEmbyUserID,
			"root", wantItemIDs[0],
		)
		if err != nil {
			t.Fatalf("error = %v, want acceptance", err)
		}
		assertEmbyItemRequests(t, cli.requests, wantItemIDs)
	})

	t.Run("root requires query beyond maximum", func(t *testing.T) {
		items := make(map[string]*emby.Item, maxEmbyParentDepth+1)
		wantItemIDs := make([]string, maxEmbyParentDepth)
		for depth := 0; depth <= maxEmbyParentDepth; depth++ {
			itemID := fmt.Sprintf("node-%03d", depth)
			parentID := fmt.Sprintf("node-%03d", depth+1)
			if depth == maxEmbyParentDepth {
				parentID = "root"
			}
			items[itemID] = &emby.Item{Id: itemID, ParentId: parentID}
			if depth < maxEmbyParentDepth {
				wantItemIDs[depth] = itemID
			}
		}

		cli := &fakeEmbyItemGetter{items: items}
		err := ValidateEmbyItemInRoot(
			context.Background(), cli, testEmbyHost, testEmbyToken, testEmbyUserID,
			"root", wantItemIDs[0],
		)
		if !errors.Is(err, errEmbyItemOutsideRoot) {
			t.Fatalf("error = %v, want shared-root rejection", err)
		}
		assertEmbyItemRequests(t, cli.requests, wantItemIDs)
	})
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
