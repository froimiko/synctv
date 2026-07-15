package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/synctv-org/synctv/internal/cache"
	"github.com/synctv-org/synctv/internal/db"
	"github.com/synctv-org/synctv/server/model"
)

func TestWriteCurrentMovieErrorLogsWrappedEmbyDiagnosticSafely(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "current-movie-secret"

	logger := log.New()
	var output strings.Builder
	logger.SetOutput(&output)
	logger.SetFormatter(&log.TextFormatter{DisableTimestamp: true, DisableQuote: true})

	cause := errors.New("https://private.example/video?api_key=" + secret)
	diagnostic := cache.NewEmbyDiagnosticErrorWithCounts(
		"media_source_processing_failed",
		cause,
		3,
		7,
		2,
	)
	err := fmt.Errorf("gen current movie info error: %w", diagnostic)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	writeCurrentMovieError(ctx, logger.WithFields(log.Fields{
		"uid":   "raw-user-id",
		"rid":   "raw-room-id",
		"token": secret,
	}), err)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
	var resp model.APIResp
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Error != "internal server error" {
		t.Fatalf("error = %q, want fixed internal error", resp.Error)
	}

	got := output.String()
	for _, field := range []string{
		"category=media_source_processing_failed",
		"source_count=3",
		"media_stream_count=7",
		"subtitle_count=2",
	} {
		if !strings.Contains(got, field) {
			t.Fatalf("log missing %q: %q", field, got)
		}
	}
	for _, sensitive := range []string{
		"private.example",
		"api_key",
		secret,
		"raw-user-id",
		"raw-room-id",
		"uid=",
		"rid=",
		"token=",
	} {
		if strings.Contains(got, sensitive) {
			t.Fatalf("log leaked %q: %q", sensitive, got)
		}
		if strings.Contains(recorder.Body.String(), sensitive) {
			t.Fatalf("response leaked %q: %s", sensitive, recorder.Body.String())
		}
	}
}

func TestWriteCurrentMovieErrorPreservesGrantDeniedResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	logger := log.New()
	var output strings.Builder
	logger.SetOutput(&output)
	logger.SetFormatter(&log.TextFormatter{DisableTimestamp: true, DisableQuote: true})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	writeCurrentMovieError(ctx, logger.WithField("rid", "raw-room-id"), fmt.Errorf(
		"gen current movie info error: %w",
		db.NewEmbyGrantError("not_found"),
	))

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", recorder.Code)
	}
	var resp model.APIResp
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Error != db.ErrEmbyGrantDenied.Error() {
		t.Fatalf("error = %q, want %q", resp.Error, db.ErrEmbyGrantDenied.Error())
	}

	got := output.String()
	if !strings.Contains(got, "category=not_found") {
		t.Fatalf("log missing grant category: %q", got)
	}
	if strings.Contains(got, "raw-room-id") || strings.Contains(got, "rid=") {
		t.Fatalf("log inherited request fields: %q", got)
	}
}
