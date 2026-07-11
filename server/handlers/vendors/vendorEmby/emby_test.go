package vendoremby

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	dbModel "github.com/synctv-org/synctv/internal/model"
	"github.com/synctv-org/synctv/internal/op"
	"github.com/synctv-org/synctv/server/model"
)

func TestProxyMovieRejectsNegativeSubtitleIndexes(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name  string
		query string
		error string
	}{
		{
			name:  "negative source",
			query: "t=subtitle&source=-1&id=0",
			error: "invalid subtitle query: source out of range",
		},
		{
			name:  "negative id",
			query: "t=subtitle&source=0&id=-1",
			error: "invalid subtitle query: id out of range",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/?"+tt.query, nil)

			service := &EmbyVendorService{}
			service.ProxyMovie(ctx)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
			}

			var resp model.APIResp
			if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp.Error != tt.error {
				t.Fatalf("error = %q, want %q", resp.Error, tt.error)
			}
			if strings.Contains(recorder.Body.String(), "api_key") {
				t.Fatalf("response leaked sensitive upstream details: %s", recorder.Body.String())
			}
		})
	}
}

func TestHandleProxyMovieRejectsNegativeSource(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/?source=-1", nil)

	service := &EmbyVendorService{
		movie: &op.Movie{Movie: &dbModel.Movie{
			MovieBase: dbModel.MovieBase{Proxy: true},
		}},
	}
	service.handleProxyMovie(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}

	var resp model.APIResp
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Error != "source out of range" {
		t.Fatalf("error = %q, want %q", resp.Error, "source out of range")
	}
}

func TestEmbySubtitleProxyURLUsesPerUserToken(t *testing.T) {
	const (
		movieID = "movie-id"
		roomID  = "room-id"
	)

	ownerURL, err := embySubtitleProxyURL(movieID, roomID, "owner-token", 1, 2)
	if err != nil {
		t.Fatalf("build owner subtitle URL: %v", err)
	}
	memberURL, err := embySubtitleProxyURL(movieID, roomID, "member-token", 1, 2)
	if err != nil {
		t.Fatalf("build member subtitle URL: %v", err)
	}

	for name, rawURL := range map[string]string{"owner": ownerURL, "member": memberURL} {
		parsed, err := url.Parse(rawURL)
		if err != nil {
			t.Fatalf("parse %s URL: %v", name, err)
		}
		if parsed.Path != "/api/room/movie/proxy/"+movieID {
			t.Errorf("%s path = %q", name, parsed.Path)
		}
		query := parsed.Query()
		if query.Get("t") != "subtitle" || query.Get("source") != "1" || query.Get("id") != "2" || query.Get("roomId") != roomID {
			t.Errorf("%s query = %q", name, parsed.RawQuery)
		}
		if strings.Contains(rawURL, "api_key") {
			t.Errorf("%s URL leaked api_key: %q", name, rawURL)
		}
	}

	if got := mustParseQuery(t, ownerURL).Get("token"); got != "owner-token" {
		t.Fatalf("owner token = %q", got)
	}
	if got := mustParseQuery(t, memberURL).Get("token"); got != "member-token" {
		t.Fatalf("member token = %q", got)
	}
}

func mustParseQuery(t *testing.T, rawURL string) url.Values {
	t.Helper()

	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	return parsed.Query()
}
