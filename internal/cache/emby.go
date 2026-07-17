package cache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/synctv-org/synctv/internal/db"
	"github.com/synctv-org/synctv/internal/model"
	"github.com/synctv-org/synctv/internal/vendor"
	"github.com/synctv-org/synctv/utils"
	"github.com/synctv-org/vendors/api/emby"
	"github.com/zijiren233/gencontainer/refreshcache"
	"github.com/zijiren233/gencontainer/refreshcache0"
	"github.com/zijiren233/gencontainer/refreshcache1"
	"github.com/zijiren233/go-uhc"
)

type EmbyUserCache = MapCache0[*EmbyUserCacheData]

type EmbyUserCacheData struct {
	Host             string
	ServerID         string
	APIKey           string
	UserID           string
	Backend          string
	BindingUpdatedAt time.Time
}

func NewEmbyUserCache(userID string) *EmbyUserCache {
	return newMapCache0(func(_ context.Context, key string) (*EmbyUserCacheData, error) {
		return EmbyAuthorizationCacheWithUserIDInitFunc(userID, key)
	}, -1)
}

func EmbyAuthorizationCacheWithUserIDInitFunc(userID, serverID string) (*EmbyUserCacheData, error) {
	if serverID == "" {
		return nil, errors.New("serverID is required")
	}

	v, err := db.GetEmbyVendor(userID, serverID)
	if err != nil {
		return nil, err
	}

	if v.APIKey == "" || v.Host == "" {
		return nil, db.NotFoundError(db.ErrVendorNotFound)
	}

	return &EmbyUserCacheData{
		Host:             v.Host,
		ServerID:         v.ServerID,
		APIKey:           v.APIKey,
		UserID:           v.EmbyUserID,
		Backend:          v.Backend,
		BindingUpdatedAt: v.UpdatedAt,
	}, nil
}

type EmbySource struct {
	URL         string
	Name        string
	Subtitles   []*EmbySubtitleCache
	IsTranscode bool
}

type EmbySubtitleCache struct {
	Cache                        *refreshcache0.RefreshCache[[]byte]
	URL                          string
	Type                         string
	Name                         string
	ContentType                  string
	RouteSource                  string
	SelectionState               string
	DeliveryURLPresent           bool
	DeliveryURLAccepted          bool
	SelectedDeliveryURLAccepted  bool
	APIPrefixAdded               bool
	FallbackAvailable            bool
	FallbackFormatState          string
	TextSubtitleState            string
	FallbackFormat               string
	SourceItemIDPresent          bool
	SourceItemIDMatchesRequested bool
	StreamItemIDPresent          bool
	StreamItemIDMatchesRequested bool
}

type EmbyMovieCacheData struct {
	TranscodeSessionID string
	Sources            []EmbySource
}

type EmbyDiagnosticDetails struct {
	Category                     string
	HTTPStatus                   int
	Timeout                      bool
	SourceCount                  int
	MediaStreamCount             int
	SubtitleCount                int
	RouteSource                  string
	SelectionState               string
	DeliveryURLPresent           bool
	DeliveryURLAccepted          bool
	SelectedDeliveryURLAccepted  bool
	APIPrefixAdded               bool
	FallbackAvailable            bool
	FallbackFormatState          string
	TextSubtitleState            string
	FallbackFormat               string
	SourceItemIDPresent          bool
	SourceItemIDMatchesRequested bool
	StreamItemIDPresent          bool
	StreamItemIDMatchesRequested bool
}

type embySubtitleRouteDetails struct {
	RouteSource                  string
	SelectionState               string
	DeliveryURLPresent           bool
	DeliveryURLAccepted          bool
	SelectedDeliveryURLAccepted  bool
	APIPrefixAdded               bool
	FallbackAvailable            bool
	FallbackFormatState          string
	TextSubtitleState            string
	FallbackFormat               string
	SourceItemIDPresent          bool
	SourceItemIDMatchesRequested bool
	StreamItemIDPresent          bool
	StreamItemIDMatchesRequested bool
}

func (route embySubtitleRouteDetails) apply(details *EmbyDiagnosticDetails) {
	details.RouteSource = route.RouteSource
	details.SelectionState = route.SelectionState
	details.DeliveryURLPresent = route.DeliveryURLPresent
	details.DeliveryURLAccepted = route.DeliveryURLAccepted
	details.SelectedDeliveryURLAccepted = route.SelectedDeliveryURLAccepted
	details.APIPrefixAdded = route.APIPrefixAdded
	details.FallbackAvailable = route.FallbackAvailable
	details.FallbackFormatState = route.FallbackFormatState
	details.TextSubtitleState = route.TextSubtitleState
	details.FallbackFormat = route.FallbackFormat
	details.SourceItemIDPresent = route.SourceItemIDPresent
	details.SourceItemIDMatchesRequested = route.SourceItemIDMatchesRequested
	details.StreamItemIDPresent = route.StreamItemIDPresent
	details.StreamItemIDMatchesRequested = route.StreamItemIDMatchesRequested
}

type embyDiagnosticError struct {
	details EmbyDiagnosticDetails
	message string
	cause   error
}

func (e *embyDiagnosticError) Error() string { return e.message }
func (e *embyDiagnosticError) Unwrap() error { return e.cause }

func newEmbyDiagnosticError(category, message string, cause error) error {
	return &embyDiagnosticError{
		details: EmbyDiagnosticDetails{
			Category:         category,
			SourceCount:      -1,
			MediaStreamCount: -1,
			SubtitleCount:    -1,
		},
		message: message,
		cause:   cause,
	}
}

func newEmbyDiagnosticErrorWithDetails(
	category, message string,
	cause error,
	configure func(*EmbyDiagnosticDetails),
) error {
	err := newEmbyDiagnosticError(category, message, cause).(*embyDiagnosticError)
	if configure != nil {
		configure(&err.details)
	}
	return err
}

func NewEmbyDiagnosticError(category string, cause error) error {
	return newEmbyDiagnosticError(category, "emby media request failed", cause)
}

func NewEmbyDiagnosticErrorWithCounts(
	category string,
	cause error,
	sourceCount, mediaStreamCount, subtitleCount int,
) error {
	return newEmbyDiagnosticErrorWithDetails(
		category,
		"emby media request failed",
		cause,
		func(details *EmbyDiagnosticDetails) {
			details.SourceCount = sourceCount
			details.MediaStreamCount = mediaStreamCount
			details.SubtitleCount = subtitleCount
		},
	)
}

func NewEmbySubtitleDiagnosticError(
	category string,
	cause error,
	subtitle *EmbySubtitleCache,
	sourceCount, subtitleCount int,
) error {
	return newEmbyDiagnosticErrorWithDetails(
		category,
		"emby media request failed",
		cause,
		func(details *EmbyDiagnosticDetails) {
			details.SourceCount = sourceCount
			details.SubtitleCount = subtitleCount
			if subtitle != nil {
				embySubtitleRouteDetails{
					RouteSource:                  subtitle.RouteSource,
					SelectionState:               subtitle.SelectionState,
					DeliveryURLPresent:           subtitle.DeliveryURLPresent,
					DeliveryURLAccepted:          subtitle.DeliveryURLAccepted,
					SelectedDeliveryURLAccepted:  subtitle.SelectedDeliveryURLAccepted,
					APIPrefixAdded:               subtitle.APIPrefixAdded,
					FallbackAvailable:            subtitle.FallbackAvailable,
					FallbackFormatState:          subtitle.FallbackFormatState,
					TextSubtitleState:            subtitle.TextSubtitleState,
					FallbackFormat:               subtitle.FallbackFormat,
					SourceItemIDPresent:          subtitle.SourceItemIDPresent,
					SourceItemIDMatchesRequested: subtitle.SourceItemIDMatchesRequested,
					StreamItemIDPresent:          subtitle.StreamItemIDPresent,
					StreamItemIDMatchesRequested: subtitle.StreamItemIDMatchesRequested,
				}.apply(details)
			}
		},
	)
}

func EmbyDiagnosticDetailsFromError(err error) (EmbyDiagnosticDetails, bool) {
	var diagnosticErr *embyDiagnosticError
	if !errors.As(err, &diagnosticErr) {
		return EmbyDiagnosticDetails{}, false
	}
	return diagnosticErr.details, true
}

func EmbyDiagnosticErrorCategory(err error) string {
	details, ok := EmbyDiagnosticDetailsFromError(err)
	if !ok {
		return ""
	}
	return details.Category
}

type EmbyMovieCache = refreshcache1.RefreshCache[*EmbyMovieCacheData, *EmbyUserCache]

func NewEmbyMovieCache(movie *model.Movie, subPath string) *EmbyMovieCache {
	cache := refreshcache1.NewRefreshCache(NewEmbyMovieCacheInitFunc(movie, subPath), -1)
	cache.SetClearFunc(NewEmbyMovieClearCacheFunc(movie, subPath))
	return cache
}

func NewEmbyMovieClearCacheFunc(
	movie *model.Movie,
	_ string,
) func(ctx context.Context, args *EmbyUserCache) error {
	return func(ctx context.Context, args *EmbyUserCache) error {
		if !movie.VendorInfo.Emby.Transcode {
			return nil
		}

		if args == nil {
			return errors.New("need emby user cache")
		}

		serverID, err := movie.VendorInfo.Emby.ServerID()
		if err != nil {
			return err
		}

		oldVal, ok := ctx.Value(refreshcache.OldValKey).(*EmbyMovieCacheData)
		if !ok {
			return nil
		}

		aucd, err := args.LoadOrStore(ctx, serverID)
		if err != nil {
			return err
		}

		if aucd.Host == "" || aucd.APIKey == "" {
			return errors.New("not bind emby vendor")
		}

		cli := vendor.LoadEmbyClient(aucd.Backend)

		_, err = cli.DeleteActiveEncodeings(ctx, &emby.DeleteActiveEncodeingsReq{
			Host:          aucd.Host,
			Token:         aucd.APIKey,
			PalySessionId: oldVal.TranscodeSessionID,
		})
		if err != nil {
			log.Errorf("delete active encodeings: %v", err)
		}

		return nil
	}
}

func ValidateEmbyMovieGrant(
	ctx context.Context,
	movie *model.Movie,
	subPath string,
	args *EmbyUserCache,
	now time.Time,
) error {
	return validateEmbyMovieGrant(ctx, movie, subPath, args, now, ValidateEmbyNavigationGrant)
}

func validateEmbyMovieGrant(
	ctx context.Context,
	movie *model.Movie,
	subPath string,
	args *EmbyUserCache,
	now time.Time,
	validateGrant func(string, string, string, string, bool, time.Time) error,
) error {
	if err := validateEmbyArgs(args, movie, subPath); err != nil {
		return err
	}
	serverID, rootItemID, requestedItemID, err := getEmbyServerIDAndPath(movie, subPath)
	if err != nil {
		return err
	}
	aucd, err := args.LoadOrStore(ctx, serverID)
	if err != nil {
		return err
	}
	if err := validateEmbyGrantBinding(aucd, serverID); err != nil {
		return err
	}
	generation, err := EmbyGrantGeneration(
		movie.ID, movie.CreatorID, aucd.Backend, serverID, rootItemID, aucd.UserID, aucd.BindingUpdatedAt,
	)
	if err != nil {
		return db.NewEmbyGrantError("invalid_generation")
	}
	return validateGrant(movie.ID, generation, rootItemID, requestedItemID, false, now)
}

func NewEmbyMovieCacheInitFunc(
	movie *model.Movie,
	subPath string,
) func(ctx context.Context, args *EmbyUserCache) (*EmbyMovieCacheData, error) {
	return newEmbyMovieCacheInitFunc(
		movie,
		subPath,
		func() time.Time { return time.Now().UTC() },
		ValidateEmbyNavigationGrant,
		vendor.LoadEmbyClient,
	)
}

func newEmbyMovieCacheInitFunc(
	movie *model.Movie,
	subPath string,
	now func() time.Time,
	validateGrant func(string, string, string, string, bool, time.Time) error,
	loadClient func(string) emby.EmbyHTTPServer,
) func(ctx context.Context, args *EmbyUserCache) (*EmbyMovieCacheData, error) {
	return func(ctx context.Context, args *EmbyUserCache) (*EmbyMovieCacheData, error) {
		if err := validateEmbyArgs(args, movie, subPath); err != nil {
			return nil, err
		}

		serverID, rootItemID, requestedItemID, err := getEmbyServerIDAndPath(movie, subPath)
		if err != nil {
			return nil, err
		}

		aucd, err := args.LoadOrStore(ctx, serverID)
		if err != nil {
			return nil, err
		}
		if err := validateEmbyGrantBinding(aucd, serverID); err != nil {
			return nil, err
		}

		generation, err := EmbyGrantGeneration(
			movie.ID,
			movie.CreatorID,
			aucd.Backend,
			serverID,
			rootItemID,
			aucd.UserID,
			aucd.BindingUpdatedAt,
		)
		if err != nil {
			return nil, db.NewEmbyGrantError("invalid_generation")
		}
		if err := validateGrant(
			movie.ID, generation, rootItemID, requestedItemID, false, now(),
		); err != nil {
			if errors.Is(err, db.ErrEmbyGrantDenied) {
				log.WithField("category", db.EmbyGrantErrorCategory(err)).Warn("emby navigation grant rejected")
			} else {
				log.WithField("category", "internal_error").Error("emby navigation grant validation failed")
			}
			return nil, err
		}

		cli := loadClient(aucd.Backend)
		data, err := getPlaybackInfo(ctx, cli, aucd, requestedItemID)
		if err != nil {
			return nil, err
		}

		return processPlaybackInfoResponseWithSelector(data, movie, aucd, requestedItemID, func(source *emby.MediaSourceInfo, stream *emby.MediaStreamInfo, route embySubtitleRouteDetails) func(context.Context) ([]byte, error) {
			return newLazyEmbySubtitleCacheInitFunc(cli, aucd, requestedItemID, source, stream, uFromHost(aucd.Host), route)
		})
	}
}

func newEmbyMediaSourceProcessingError(
	message string,
	cause error,
	mediaSources []*emby.MediaSourceInfo,
) error {
	mediaStreamCount := 0
	subtitleCount := 0
	for _, source := range mediaSources {
		if source == nil {
			continue
		}
		mediaStreamCount += len(source.GetMediaStreamInfo())
		for _, stream := range source.GetMediaStreamInfo() {
			if stream != nil && stream.GetType() == "Subtitle" {
				subtitleCount++
			}
		}
	}

	return newEmbyDiagnosticErrorWithDetails(
		"media_source_processing_failed",
		message,
		cause,
		func(details *EmbyDiagnosticDetails) {
			details.SourceCount = len(mediaSources)
			details.MediaStreamCount = mediaStreamCount
			details.SubtitleCount = subtitleCount
		},
	)
}

func processPlaybackInfoResponse(
	data *emby.PlaybackInfoResp,
	movie *model.Movie,
	aucd *EmbyUserCacheData,
	requestedItemID string,
) (*EmbyMovieCacheData, error) {
	return processPlaybackInfoResponseWithSelector(data, movie, aucd, requestedItemID, nil)
}

func processPlaybackInfoResponseWithSelector(
	data *emby.PlaybackInfoResp,
	movie *model.Movie,
	aucd *EmbyUserCacheData,
	requestedItemID string,
	lazyInit func(*emby.MediaSourceInfo, *emby.MediaStreamInfo, embySubtitleRouteDetails) func(context.Context) ([]byte, error),
) (*EmbyMovieCacheData, error) {
	if data == nil {
		return nil, newEmbyDiagnosticError("playback_info_empty", "playback info response is empty", nil)
	}

	mediaSources := data.GetMediaSourceInfo()
	if len(mediaSources) == 0 {
		return nil, newEmbyDiagnosticErrorWithDetails(
			"playback_info_empty",
			"playback info response is empty",
			nil,
			func(details *EmbyDiagnosticDetails) {
				details.SourceCount = 0
			},
		)
	}

	resp := &EmbyMovieCacheData{
		Sources:            make([]EmbySource, len(mediaSources)),
		TranscodeSessionID: data.GetPlaySessionID(),
	}

	u, err := url.Parse(aucd.Host)
	if err != nil {
		return nil, newEmbyMediaSourceProcessingError("failed to process media sources", err, mediaSources)
	}

	validSourceCount := 0
	for i, v := range mediaSources {
		source, err := processMediaSource(v, movie, aucd, requestedItemID, u)
		if err != nil {
			return nil, newEmbyMediaSourceProcessingError("failed to process media source", err, mediaSources)
		}
		if source == nil {
			continue
		}

		validSourceCount++
		resp.Sources[i] = *source
		resp.Sources[i].Subtitles = processEmbySubtitles(v, requestedItemID, aucd.APIKey, u, lazyInit)
	}
	if validSourceCount == 0 {
		return nil, newEmbyMediaSourceProcessingError("failed to process media sources", nil, mediaSources)
	}

	return resp, nil
}

func validateEmbyArgs(args *EmbyUserCache, movie *model.Movie, subPath string) error {
	if args == nil {
		return errors.New("need emby user cache")
	}
	if movie == nil {
		return errors.New("need emby movie")
	}
	if movie.VendorInfo.Emby == nil {
		return errors.New("invalid emby movie configuration")
	}

	if movie.IsFolder && subPath == "" {
		return errors.New("sub path is empty")
	}

	return nil
}

func validateEmbyGrantBinding(aucd *EmbyUserCacheData, serverID string) error {
	switch {
	case aucd == nil:
		return errors.New("missing emby binding")
	case aucd.Host == "" || aucd.APIKey == "":
		return errors.New("not bind emby vendor")
	case aucd.ServerID == "" || aucd.ServerID != serverID:
		return errors.New("invalid emby binding server")
	}
	return nil
}

func getEmbyServerIDAndPath(movie *model.Movie, subPath string) (string, string, string, error) {
	serverID, rootItemID, err := movie.VendorInfo.Emby.ServerIDAndFilePath()
	if err != nil {
		return "", "", "", err
	}

	requestedItemID := rootItemID
	if movie.IsFolder {
		requestedItemID = subPath
	}

	return serverID, rootItemID, requestedItemID, nil
}

func getPlaybackInfo(
	ctx context.Context,
	cli emby.EmbyHTTPServer,
	aucd *EmbyUserCacheData,
	truePath string,
) (*emby.PlaybackInfoResp, error) {
	if cli == nil || aucd == nil {
		return nil, newEmbyDiagnosticError(
			"playback_info_failed",
			"playback info request failed",
			errors.New("invalid playback info context"),
		)
	}
	data, err := cli.PlaybackInfo(ctx, &emby.PlaybackInfoReq{
		Host:   aucd.Host,
		Token:  aucd.APIKey,
		UserId: aucd.UserID,
		ItemId: truePath,
	})
	if err != nil {
		return nil, newEmbyDiagnosticError("playback_info_failed", "playback info request failed", err)
	}
	return data, nil
}

func getSelectedPlaybackInfo(
	ctx context.Context,
	cli emby.EmbyHTTPServer,
	aucd *EmbyUserCacheData,
	itemID, sourceID string,
	streamIndex uint64,
) (string, error) {
	if streamIndex > math.MaxInt32 {
		return "", newEmbyDiagnosticError("subtitle_stream_index_invalid", "subtitle stream index is invalid", nil)
	}
	index := int32(streamIndex)
	data, err := cli.PlaybackInfo(ctx, &emby.PlaybackInfoReq{
		Host: aucd.Host, Token: aucd.APIKey, UserId: aucd.UserID, ItemId: itemID,
		MediaSourceId: sourceID, SubtitleStreamIndex: &index, NonPlayback: true,
	})
	if err != nil {
		return "", newEmbyDiagnosticError("playback_info_failed", "selected playback info request failed", err)
	}
	if data == nil {
		return "", newEmbyDiagnosticError("playback_info_empty", "selected playback info response is empty", nil)
	}

	selectedDeliveryURL := ""
	for _, source := range data.GetMediaSourceInfo() {
		if source == nil || source.GetId() != sourceID {
			continue
		}
		for _, stream := range source.GetMediaStreamInfo() {
			if stream != nil && stream.GetType() == "Subtitle" && stream.GetIndex() == streamIndex {
				selectedDeliveryURL = stream.GetDeliveryUrl()
				break
			}
		}
	}

	if sessionID := data.GetPlaySessionID(); sessionID != "" {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), embySelectedSessionCleanupTimeout)
		_, cleanupErr := cli.DeleteActiveEncodeings(cleanupCtx, &emby.DeleteActiveEncodeingsReq{
			Host: aucd.Host, Token: aucd.APIKey, PalySessionId: sessionID,
		})
		cleanupCancel()
		if cleanupErr != nil {
			log.Warn("failed to clean selected playback info session")
		}
	}
	return selectedDeliveryURL, nil
}

func processMediaSource(
	v *emby.MediaSourceInfo,
	_ *model.Movie,
	aucd *EmbyUserCacheData,
	truePath string,
	u *url.URL,
) (*EmbySource, error) {
	source := &EmbySource{Name: v.GetName()}

	switch {
	case v.GetTranscodingUrl() != "":
		source.URL = fmt.Sprintf("%s/emby%s", aucd.Host, v.GetTranscodingUrl())
		source.IsTranscode = true
	case v.GetDirectPlayUrl() != "":
		source.URL = fmt.Sprintf("%s/emby%s", aucd.Host, v.GetDirectPlayUrl())
		source.IsTranscode = false
	default:
		if v.GetContainer() == "" {
			return nil, nil
		}

		result, err := url.JoinPath("emby", "Videos", truePath, "stream."+v.GetContainer())
		if err != nil {
			return nil, err
		}

		sourceURL := *u
		sourceURL.Path = result
		sourceURL.RawPath = ""
		query := url.Values{}
		query.Set("api_key", aucd.APIKey)
		query.Set("Static", "true")
		query.Set("MediaSourceId", v.GetId())
		sourceURL.RawQuery = query.Encode()
		source.URL = sourceURL.String()
	}

	return source, nil
}

func embyEffectivePort(u *url.URL) (int, bool) {
	if port := u.Port(); port != "" {
		value, err := strconv.Atoi(port)
		return value, err == nil && value > 0 && value <= 65535
	}
	if strings.EqualFold(u.Scheme, "https") {
		return 443, true
	}
	if strings.EqualFold(u.Scheme, "http") {
		return 80, true
	}
	return 0, false
}

func validEmbySubtitleBaseURL(base *url.URL) bool {
	if base == nil || base.User != nil || base.Fragment != "" || base.Hostname() == "" ||
		!strings.EqualFold(base.Scheme, "http") && !strings.EqualFold(base.Scheme, "https") {
		return false
	}
	_, ok := embyEffectivePort(base)
	return ok
}

func setCanonicalEmbyAPIKey(u *url.URL, query url.Values, apiKey string) {
	for key := range query {
		if strings.EqualFold(key, "api_key") || strings.EqualFold(key, "x-emby-token") {
			query.Del(key)
		}
	}
	query.Set("api_key", apiKey)
	u.RawQuery = query.Encode()
}

func embySubtitleDeliveryURL(base *url.URL, rawURL, apiKey string) (*url.URL, bool, bool) {
	if rawURL == "" || !validEmbySubtitleBaseURL(base) {
		return nil, false, false
	}

	reference, err := url.Parse(rawURL)
	if err != nil {
		return nil, false, false
	}
	resolved := base.ResolveReference(reference)
	if resolved.User != nil || resolved.Fragment != "" || resolved.Hostname() == "" ||
		(!strings.EqualFold(resolved.Scheme, "http") && !strings.EqualFold(resolved.Scheme, "https")) ||
		!strings.EqualFold(resolved.Scheme, base.Scheme) ||
		!strings.EqualFold(resolved.Hostname(), base.Hostname()) {
		return nil, false, false
	}

	basePort, basePortOK := embyEffectivePort(base)
	resolvedPort, resolvedPortOK := embyEffectivePort(resolved)
	if !basePortOK || !resolvedPortOK || basePort != resolvedPort {
		return nil, false, false
	}

	query, err := url.ParseQuery(resolved.RawQuery)
	if err != nil {
		return nil, false, false
	}
	apiPrefixAdded := resolved.Path == "/Videos" || strings.HasPrefix(resolved.Path, "/Videos/")
	if apiPrefixAdded {
		resolved.Path = "/emby" + resolved.Path
		if resolved.RawPath != "" {
			resolved.RawPath = "/emby" + resolved.RawPath
		}
	}
	setCanonicalEmbyAPIKey(resolved, query, apiKey)
	return resolved, true, apiPrefixAdded
}

func embySubtitleFormatValue(value string) (string, string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "vtt", "webvtt", "text/vtt", "text/webvtt":
		return "vtt", "text/vtt; charset=utf-8", true
	case "srt", "subrip", "application/x-subrip", "application/subrip", "text/srt":
		return "srt", "application/x-subrip; charset=utf-8", true
	case "ass", "ssa", "text/x-ssa", "text/ssa", "text/x-ass", "application/x-ass":
		return "ass", "text/x-ssa; charset=utf-8", true
	default:
		return "", "", false
	}
}

func embySubtitleFormat(deliveryURL *url.URL, codec, mimeType string) (string, string) {
	if deliveryURL != nil {
		extension := strings.TrimPrefix(path.Ext(deliveryURL.Path), ".")
		if subtitleType, contentType, ok := embySubtitleFormatValue(extension); ok {
			return subtitleType, contentType
		}
	}
	if subtitleType, contentType, ok := embySubtitleFormatValue(codec); ok {
		return subtitleType, contentType
	}
	if mediaType, _, err := mime.ParseMediaType(mimeType); err == nil {
		if subtitleType, contentType, ok := embySubtitleFormatValue(mediaType); ok {
			return subtitleType, contentType
		}
	}
	return "vtt", "text/vtt; charset=utf-8"
}

func embySubtitleFallbackFormat(codec, mimeType string) (string, string, bool) {
	if subtitleType, contentType, ok := embySubtitleFormatValue(codec); ok {
		return subtitleType, contentType, true
	}
	if mediaType, _, err := mime.ParseMediaType(mimeType); err == nil {
		return embySubtitleFormatValue(mediaType)
	}
	return "", "", false
}

func embySubtitleFallbackFormatState(codec, mimeType string) string {
	if strings.TrimSpace(codec) == "" && strings.TrimSpace(mimeType) == "" {
		return "missing"
	}
	if _, _, ok := embySubtitleFallbackFormat(codec, mimeType); ok {
		return "supported"
	}
	return "unsupported"
}

func embySubtitleFallbackURL(base *url.URL, itemID, sourceID string, index uint64, apiKey, subtitleType string) (*url.URL, bool) {
	if !validEmbySubtitleBaseURL(base) {
		return nil, false
	}

	fallback := *base
	reference := &url.URL{Path: path.Join(
		"/emby", "Videos", itemID, sourceID, "Subtitles", strconv.FormatUint(index, 10), "Stream."+subtitleType,
	)}
	fallback = *base.ResolveReference(reference)
	fallback.RawPath = ""
	fallback.RawQuery = ""
	fallback.Fragment = ""
	setCanonicalEmbyAPIKey(&fallback, url.Values{}, apiKey)
	return &fallback, true
}

func processEmbySubtitles(
	v *emby.MediaSourceInfo,
	truePath string,
	apiKey string,
	base *url.URL,
	lazyInitializers ...func(*emby.MediaSourceInfo, *emby.MediaStreamInfo, embySubtitleRouteDetails) func(context.Context) ([]byte, error),
) []*EmbySubtitleCache {
	var lazyInit func(*emby.MediaSourceInfo, *emby.MediaStreamInfo, embySubtitleRouteDetails) func(context.Context) ([]byte, error)
	if len(lazyInitializers) != 0 {
		lazyInit = lazyInitializers[0]
	}
	if v == nil {
		return nil
	}

	subtitles := make([]*EmbySubtitleCache, 0, len(v.GetMediaStreamInfo()))
	for _, msi := range v.GetMediaStreamInfo() {
		if msi == nil || msi.GetType() != "Subtitle" {
			continue
		}

		textSubtitleState := "non_text"
		if msi.GetIsTextSubtitleStream() {
			textSubtitleState = "text"
		}
		subtitleType := ""
		contentType := ""
		subtitleURLString := ""
		route := embySubtitleRouteDetails{
			RouteSource:                  "none",
			DeliveryURLPresent:           msi.GetDeliveryUrl() != "",
			FallbackFormatState:          embySubtitleFallbackFormatState(msi.GetCodec(), msi.GetMimeType()),
			TextSubtitleState:            textSubtitleState,
			FallbackFormat:               "none",
			SourceItemIDPresent:          v.GetItemIdPresent(),
			SourceItemIDMatchesRequested: v.GetItemIdMatchesRequested(),
			StreamItemIDPresent:          msi.GetItemIdPresent(),
			StreamItemIDMatchesRequested: msi.GetItemIdMatchesRequested(),
		}
		rawDeliveryURL := msi.GetDeliveryUrl()
		if deliveryURL, ok, apiPrefixAdded := embySubtitleDeliveryURL(base, rawDeliveryURL, apiKey); ok {
			subtitleURLString = deliveryURL.String()
			subtitleType, contentType = embySubtitleFormat(deliveryURL, msi.GetCodec(), msi.GetMimeType())
			route.RouteSource = "delivery_url"
			route.DeliveryURLAccepted = true
			route.APIPrefixAdded = apiPrefixAdded
		} else if msi.GetIsTextSubtitleStream() {
			if fallbackType, fallbackContentType, supported := embySubtitleFallbackFormat(msi.GetCodec(), msi.GetMimeType()); supported {
				if fallback, ok := embySubtitleFallbackURL(base, truePath, v.GetId(), msi.GetIndex(), apiKey, fallbackType); ok {
					subtitleURLString = fallback.String()
					subtitleType = fallbackType
					contentType = fallbackContentType
					route.RouteSource = "vtt_fallback"
					route.FallbackAvailable = true
					route.FallbackFormat = fallbackType
				}
			}
		}

		name := msi.GetDisplayTitle()
		if name == "" {
			if msi.GetTitle() != "" {
				name = msi.GetTitle()
			} else {
				name = msi.GetDisplayLanguage()
			}
		}

		initFunc := newEmbySubtitleCacheInitFunc(subtitleURLString, route)
		if rawDeliveryURL == "" && lazyInit != nil {
			initFunc = lazyInit(v, msi, route)
		} else {
			route.SelectionState = "selected_skipped"
		}
		subtitles = append(subtitles, &EmbySubtitleCache{
			URL:                          subtitleURLString,
			Type:                         subtitleType,
			Name:                         name,
			ContentType:                  contentType,
			RouteSource:                  route.RouteSource,
			SelectionState:               route.SelectionState,
			DeliveryURLPresent:           route.DeliveryURLPresent,
			DeliveryURLAccepted:          route.DeliveryURLAccepted,
			SelectedDeliveryURLAccepted:  route.SelectedDeliveryURLAccepted,
			APIPrefixAdded:               route.APIPrefixAdded,
			FallbackAvailable:            route.FallbackAvailable,
			FallbackFormatState:          route.FallbackFormatState,
			TextSubtitleState:            route.TextSubtitleState,
			FallbackFormat:               route.FallbackFormat,
			SourceItemIDPresent:          route.SourceItemIDPresent,
			SourceItemIDMatchesRequested: route.SourceItemIDMatchesRequested,
			StreamItemIDPresent:          route.StreamItemIDPresent,
			StreamItemIDMatchesRequested: route.StreamItemIDMatchesRequested,
			Cache:                        refreshcache0.NewRefreshCache(initFunc, -1),
		})
	}

	return subtitles
}

const (
	embySubtitleProbeEnv              = "SYNCTV_EMBY_SUBTITLE_PROBE"
	embySelectedSessionCleanupTimeout = 2 * time.Second
)

var (
	embySRTCue      = regexp.MustCompile(`(?s)^(?:\d+[ \t]*\r?\n)?[ \t]*\d{1,2}:\d{2}:\d{2},\d{3}[ \t]+-->[ \t]+\d{1,2}:\d{2}:\d{2},\d{3}[^\r\n]*\r?\n[^\r\n]+(?:\r?\n|$)`)
	embyMarkupStart = regexp.MustCompile(`(?is)^\s*<\??[a-z!/]`)
)

func uFromHost(host string) *url.URL {
	u, _ := url.Parse(host)
	return u
}

func stripUTF8BOM(data []byte) []byte {
	return bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
}

func looksLikeEmbySubtitle(data []byte) bool {
	body := stripUTF8BOM(data)
	if len(body) == 0 {
		return false
	}

	firstLine := body
	if newline := bytes.IndexByte(firstLine, '\n'); newline >= 0 {
		firstLine = firstLine[:newline]
	}
	firstLine = bytes.TrimSuffix(firstLine, []byte{'\r'})
	if bytes.Equal(bytes.ToUpper(firstLine), []byte("WEBVTT")) {
		return true
	}

	return embySRTCue.Match(body)
}

func embySubtitleContentTypeClass(value string) string {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil || mediaType == "" {
		return "unknown"
	}
	switch {
	case strings.Contains(mediaType, "html") || strings.Contains(mediaType, "xml"):
		return "markup"
	case strings.Contains(mediaType, "json"):
		return "json"
	case strings.HasPrefix(mediaType, "text/") || strings.Contains(mediaType, "subrip"):
		return "text"
	default:
		return "other"
	}
}

func embySubtitleBodyClass(data []byte) string {
	trimmed := bytes.TrimSpace(stripUTF8BOM(data))
	lower := bytes.ToLower(trimmed)
	switch {
	case len(trimmed) == 0:
		return "empty"
	case embyMarkupStart.Match(trimmed):
		return "markup"
	case trimmed[0] == '{' || trimmed[0] == '[' || bytes.HasPrefix(lower, []byte(")]}'")) || bytes.HasPrefix(lower, []byte("while(1);")) || bytes.HasPrefix(lower, []byte("for(;;);")):
		return "json"
	case looksLikeEmbySubtitle(trimmed):
		return "subtitle"
	default:
		return "other"
	}
}

type embySubtitleProbeCandidate struct {
	name string
	url  string
}

func newLazyEmbySubtitleCacheInitFunc(cli emby.EmbyHTTPServer, aucd *EmbyUserCacheData, itemID string, source *emby.MediaSourceInfo, stream *emby.MediaStreamInfo, base *url.URL, route embySubtitleRouteDetails) func(context.Context) ([]byte, error) {
	return func(ctx context.Context) ([]byte, error) {
		format, _, supported := embySubtitleFallbackFormat(stream.GetCodec(), stream.GetMimeType())
		if !supported {
			route.SelectionState = "selected_skipped"
			return nil, newEmbyDiagnosticErrorWithDetails("subtitle_cache_fetch_failed", "subtitle cache fetch failed", nil, route.apply)
		}
		primary, _ := embySubtitleFallbackURL(base, itemID, source.GetId(), stream.GetIndex(), aucd.APIKey, format)
		if os.Getenv(embySubtitleProbeEnv) != "1" || format == "ass" {
			route.SelectionState = "selected_skipped"
			return newEmbySubtitleCacheInitFunc(primary.String(), route)(ctx)
		}

		probeCtx, cancel := context.WithTimeout(ctx, embySubtitleHTTPTimeout)
		defer cancel()
		client := newEmbySubtitleHTTPClient()
		client.Timeout = 0 // The shared probe context owns the total 30-second budget.
		selected, selectionErr := getSelectedPlaybackInfo(probeCtx, cli, aucd, itemID, source.GetId(), stream.GetIndex())
		route.SelectionState = "selected_no_delivery"
		if selectionErr != nil {
			route.SelectionState = "selected_error"
		} else if selected != "" {
			route.SelectionState = "selected_delivery_url"
		}
		log.WithField("selection_state", route.SelectionState).Info("emby subtitle selected playback info")
		if selectedURL, ok, apiPrefixAdded := embySubtitleDeliveryURL(base, selected, aucd.APIKey); ok {
			route.RouteSource = "delivery_url"
			route.SelectedDeliveryURLAccepted = true
			route.APIPrefixAdded = apiPrefixAdded
			data, _, _, _, fetchErr := fetchEmbySubtitleCandidate(probeCtx, client, selectedURL.String())
			if fetchErr != nil {
				return nil, newEmbySubtitleProbeDiagnosticError(probeCtx, route, fetchErr, nil)
			}
			return data, nil
		}
		if probeCtx.Err() != nil {
			return nil, newEmbyDiagnosticErrorWithDetails("subtitle_upstream_timeout", "emby media request failed", probeCtx.Err(), route.apply)
		}

		makeURL := func(withSource bool, extension string) string {
			parts := []string{"/emby", "Videos", itemID}
			if withSource {
				parts = append(parts, source.GetId())
			}
			parts = append(parts, "Subtitles", strconv.FormatUint(stream.GetIndex(), 10), "Stream."+extension)
			u := *base
			u.Path = path.Join(parts...)
			u.RawQuery = ""
			setCanonicalEmbyAPIKey(&u, url.Values{}, aucd.APIKey)
			return u.String()
		}
		rawCandidates := []embySubtitleProbeCandidate{
			{"with_source_original", makeURL(true, format)},
			{"with_source_vtt", makeURL(true, "vtt")},
			{"without_source_original", makeURL(false, format)},
			{"without_source_vtt", makeURL(false, "vtt")},
		}
		candidates := make([]embySubtitleProbeCandidate, 0, len(rawCandidates))
		seen := make(map[string]struct{}, len(rawCandidates))
		for _, candidate := range rawCandidates {
			if _, exists := seen[candidate.url]; exists {
				continue
			}
			seen[candidate.url] = struct{}{}
			candidates = append(candidates, candidate)
		}
		var lastErr error
		for _, candidate := range candidates {
			if err := probeCtx.Err(); err != nil {
				return nil, newEmbySubtitleProbeDiagnosticError(probeCtx, route, lastErr, err)
			}
			data, status, contentClass, bodyClass, fetchErr := fetchEmbySubtitleCandidate(probeCtx, client, candidate.url)
			log.WithFields(log.Fields{"candidate": candidate.name, "status": status, "content_type_class": contentClass, "body_class": bodyClass}).Info("emby subtitle probe")
			if fetchErr == nil {
				return data, nil
			}
			lastErr = fetchErr
		}
		return nil, newEmbySubtitleProbeDiagnosticError(probeCtx, route, lastErr, nil)
	}
}

const embySubtitleHTTPTimeout = 30 * time.Second

func newEmbySubtitleHTTPClient() *http.Client {
	return &http.Client{
		Transport: uhc.DefaultTransport,
		Timeout:   embySubtitleHTTPTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func newEmbySubtitleProbeDiagnosticError(ctx context.Context, route embySubtitleRouteDetails, fetchErr, contextErr error) error {
	if contextErr == nil && ctx != nil {
		contextErr = ctx.Err()
	}
	if errors.Is(contextErr, context.DeadlineExceeded) {
		return newEmbyDiagnosticErrorWithDetails(
			"subtitle_upstream_timeout", "emby media request failed", contextErr,
			func(details *EmbyDiagnosticDetails) {
				route.apply(details)
				details.Timeout = true
			},
		)
	}
	if fetchErr == nil {
		fetchErr = contextErr
	}
	if details, ok := EmbyDiagnosticDetailsFromError(fetchErr); ok {
		return newEmbyDiagnosticErrorWithDetails(
			details.Category, "emby media request failed", fetchErr,
			func(wrapped *EmbyDiagnosticDetails) {
				route.apply(wrapped)
				wrapped.HTTPStatus = details.HTTPStatus
				wrapped.Timeout = details.Timeout
			},
		)
	}
	return newEmbyDiagnosticErrorWithDetails(
		"subtitle_upstream_request_failed", "emby media request failed", fetchErr, route.apply,
	)
}

func fetchEmbySubtitleCandidate(ctx context.Context, client *http.Client, rawURL string) ([]byte, int, string, string, error) {
	if client == nil {
		client = newEmbySubtitleHTTPClient()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, 0, "unknown", "unread", NewEmbyDiagnosticError("subtitle_upstream_request_failed", err)
	}
	req.Header.Set("User-Agent", utils.UA)
	req.Header.Set("Referer", req.URL.Scheme+"://"+req.URL.Host)
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, "unknown", "unread", NewEmbyDiagnosticError("subtitle_upstream_request_failed", err)
	}
	defer resp.Body.Close()
	contentClass := embySubtitleContentTypeClass(resp.Header.Get("Content-Type"))
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, contentClass, "unread", newEmbyDiagnosticErrorWithDetails(
			"subtitle_upstream_status", "emby media request failed", nil,
			func(details *EmbyDiagnosticDetails) { details.HTTPStatus = resp.StatusCode },
		)
	}
	if resp.ContentLength > subtitleMaxLength {
		return nil, resp.StatusCode, contentClass, "too_large", NewEmbyDiagnosticError("subtitle_response_too_large", nil)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, subtitleMaxLength+1))
	if err != nil {
		return nil, resp.StatusCode, contentClass, "unread", NewEmbyDiagnosticError("subtitle_upstream_read_failed", err)
	}
	if len(data) > subtitleMaxLength {
		return nil, resp.StatusCode, contentClass, "too_large", NewEmbyDiagnosticError("subtitle_response_too_large", nil)
	}
	bodyClass := embySubtitleBodyClass(data)
	if contentClass == "markup" || contentClass == "json" || bodyClass != "subtitle" {
		return nil, resp.StatusCode, contentClass, bodyClass, NewEmbyDiagnosticError("subtitle_invalid_body", nil)
	}
	return data, resp.StatusCode, contentClass, bodyClass, nil
}

func newEmbySubtitleCacheInitFunc(rawURL string, routes ...embySubtitleRouteDetails) func(ctx context.Context) ([]byte, error) {
	client := newEmbySubtitleHTTPClient()
	route := embySubtitleRouteDetails{RouteSource: "none"}
	if len(routes) != 0 {
		route = routes[0]
	}
	newDiagnosticError := func(category, message string, cause error, configure func(*EmbyDiagnosticDetails)) error {
		return newEmbyDiagnosticErrorWithDetails(category, message, cause, func(details *EmbyDiagnosticDetails) {
			route.apply(details)
			if configure != nil {
				configure(details)
			}
		})
	}
	return func(ctx context.Context) ([]byte, error) {
		if rawURL == "" {
			return nil, newDiagnosticError(
				"subtitle_cache_fetch_failed", "subtitle cache fetch failed", nil, nil,
			)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, newDiagnosticError(
				"subtitle_upstream_request_failed", "failed to create subtitle request", err, nil,
			)
		}

		req.Header.Set("User-Agent", utils.UA)
		req.Header.Set("Referer", req.URL.Scheme+"://"+req.URL.Host)

		resp, err := client.Do(req)
		if err != nil {
			timeout := false
			category := "subtitle_upstream_request_failed"
			var netErr interface{ Timeout() bool }
			if errors.As(err, &netErr) && netErr.Timeout() {
				timeout = true
				category = "subtitle_upstream_timeout"
			}
			return nil, newDiagnosticError(
				category,
				"failed to fetch subtitle",
				err,
				func(details *EmbyDiagnosticDetails) {
					details.Timeout = timeout
				},
			)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, newDiagnosticError(
				"subtitle_upstream_status",
				fmt.Sprintf("unexpected subtitle status: %d", resp.StatusCode),
				nil,
				func(details *EmbyDiagnosticDetails) {
					details.HTTPStatus = resp.StatusCode
				},
			)
		}
		if resp.ContentLength > subtitleMaxLength {
			return nil, newDiagnosticError(
				"subtitle_response_too_large",
				"subtitle response too large",
				nil,
				nil,
			)
		}

		data, err := io.ReadAll(io.LimitReader(resp.Body, subtitleMaxLength+1))
		if err != nil {
			return nil, newDiagnosticError(
				"subtitle_upstream_read_failed",
				"failed to read subtitle",
				err,
				nil,
			)
		}
		if len(data) > subtitleMaxLength {
			return nil, newDiagnosticError(
				"subtitle_response_too_large",
				"subtitle response too large",
				nil,
				nil,
			)
		}
		return data, nil
	}
}
