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

type fakeEmbyItemGetter struct {
	items map[string]*emby.Item
	err   error
	calls int
}

func (f *fakeEmbyItemGetter) GetItem(
	_ context.Context,
	req *emby.GetItemReq,
) (*emby.Item, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.items[req.GetItemId()], nil
}

func TestValidateEmbyItemInRoot(t *testing.T) {
	tests := []struct {
		name          string
		rootItemID    string
		requestedID   string
		items         map[string]*emby.Item
		getItemErr    error
		wantErr       bool
		wantItemCalls int
	}{
		{
			name:          "root itself",
			rootItemID:    "root",
			requestedID:   "root",
			wantItemCalls: 0,
		},
		{
			name:          "empty root",
			requestedID:   "child",
			wantErr:       true,
			wantItemCalls: 0,
		},
		{
			name:          "empty request",
			rootItemID:    "root",
			wantErr:       true,
			wantItemCalls: 0,
		},
		{
			name:        "direct descendant",
			rootItemID:  "root",
			requestedID: "child",
			items: map[string]*emby.Item{
				"child": {Id: "child", ParentId: "root"},
			},
			wantItemCalls: 1,
		},
		{
			name:        "multi level descendant",
			rootItemID:  "root",
			requestedID: "grandchild",
			items: map[string]*emby.Item{
				"grandchild": {Id: "grandchild", ParentId: "child"},
				"child":      {Id: "child", ParentId: "root"},
			},
			wantItemCalls: 2,
		},
		{
			name:        "unrelated root",
			rootItemID:  "root",
			requestedID: "child",
			items: map[string]*emby.Item{
				"child": {Id: "child", ParentId: "other"},
				"other": {Id: "other"},
			},
			wantErr:       true,
			wantItemCalls: 2,
		},
		{
			name:        "empty parent",
			rootItemID:  "root",
			requestedID: "child",
			items: map[string]*emby.Item{
				"child": {Id: "child"},
			},
			wantErr:       true,
			wantItemCalls: 1,
		},
		{
			name:        "cycle",
			rootItemID:  "root",
			requestedID: "child",
			items: map[string]*emby.Item{
				"child": {Id: "child", ParentId: "parent"},
				"parent": {Id: "parent", ParentId: "child"},
			},
			wantErr:       true,
			wantItemCalls: 2,
		},
		{
			name:        "returned ID mismatch",
			rootItemID:  "root",
			requestedID: "child",
			items: map[string]*emby.Item{
				"child": {Id: "different", ParentId: "root"},
			},
			wantErr:       true,
			wantItemCalls: 1,
		},
		{
			name:          "GetItem error",
			rootItemID:    "root",
			requestedID:   "child",
			getItemErr:    errors.New("upstream failure"),
			wantErr:       true,
			wantItemCalls: 1,
		},
		{
			name:          "nil item",
			rootItemID:    "root",
			requestedID:   "missing",
			items:         map[string]*emby.Item{},
			wantErr:       true,
			wantItemCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cli := &fakeEmbyItemGetter{items: tt.items, err: tt.getItemErr}
			err := ValidateEmbyItemInRoot(
				context.Background(), cli, "https://emby.example", "secret", tt.rootItemID, tt.requestedID,
			)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validation error state mismatch")
			}
			if cli.calls != tt.wantItemCalls {
				t.Fatalf("GetItem calls = %d, want %d", cli.calls, tt.wantItemCalls)
			}
			if err != nil {
				for _, sensitive := range []string{"https://emby.example", "secret"} {
					if strings.Contains(err.Error(), sensitive) {
						t.Fatalf("validation error leaked credentials")
					}
				}
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
