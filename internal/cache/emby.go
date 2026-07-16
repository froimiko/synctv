package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
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
	Cache               *refreshcache0.RefreshCache[[]byte]
	URL                 string
	Type                string
	Name                string
	ContentType         string
	RouteSource         string
	DeliveryURLPresent  bool
	DeliveryURLAccepted bool
	APIPrefixAdded      bool
	FallbackAvailable   bool
}

type EmbyMovieCacheData struct {
	TranscodeSessionID string
	Sources            []EmbySource
}

type EmbyDiagnosticDetails struct {
	Category            string
	HTTPStatus          int
	Timeout             bool
	SourceCount         int
	MediaStreamCount    int
	SubtitleCount       int
	RouteSource         string
	DeliveryURLPresent  bool
	DeliveryURLAccepted bool
	APIPrefixAdded      bool
	FallbackAvailable   bool
}

type embySubtitleRouteDetails struct {
	RouteSource         string
	DeliveryURLPresent  bool
	DeliveryURLAccepted bool
	APIPrefixAdded      bool
	FallbackAvailable   bool
}

func (route embySubtitleRouteDetails) apply(details *EmbyDiagnosticDetails) {
	details.RouteSource = route.RouteSource
	details.DeliveryURLPresent = route.DeliveryURLPresent
	details.DeliveryURLAccepted = route.DeliveryURLAccepted
	details.APIPrefixAdded = route.APIPrefixAdded
	details.FallbackAvailable = route.FallbackAvailable
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
					RouteSource:         subtitle.RouteSource,
					DeliveryURLPresent:  subtitle.DeliveryURLPresent,
					DeliveryURLAccepted: subtitle.DeliveryURLAccepted,
					APIPrefixAdded:      subtitle.APIPrefixAdded,
					FallbackAvailable:   subtitle.FallbackAvailable,
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

		return processPlaybackInfoResponse(data, movie, aucd, requestedItemID)
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
		resp.Sources[i].Subtitles = processEmbySubtitles(v, requestedItemID, aucd.APIKey, u)
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

func embySubtitleFallbackURL(base *url.URL, itemID, sourceID string, index uint64, apiKey string) (*url.URL, bool) {
	if !validEmbySubtitleBaseURL(base) {
		return nil, false
	}

	fallback := *base
	reference := &url.URL{Path: path.Join(
		"/emby", "Videos", itemID, sourceID, "Subtitles", strconv.FormatUint(index, 10), "Stream.vtt",
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
) []*EmbySubtitleCache {
	if v == nil {
		return nil
	}

	subtitles := make([]*EmbySubtitleCache, 0, len(v.GetMediaStreamInfo()))
	for _, msi := range v.GetMediaStreamInfo() {
		if msi == nil || msi.GetType() != "Subtitle" {
			continue
		}

		subtitleType := "vtt"
		contentType := "text/vtt; charset=utf-8"
		subtitleURLString := ""
		route := embySubtitleRouteDetails{
			RouteSource:        "none",
			DeliveryURLPresent: msi.GetDeliveryUrl() != "",
		}
		if fallback, ok := embySubtitleFallbackURL(base, truePath, v.GetId(), msi.GetIndex(), apiKey); ok {
			subtitleURLString = fallback.String()
			route.RouteSource = "vtt_fallback"
			route.FallbackAvailable = true
		}
		if deliveryURL, ok, apiPrefixAdded := embySubtitleDeliveryURL(base, msi.GetDeliveryUrl(), apiKey); ok {
			subtitleURLString = deliveryURL.String()
			subtitleType, contentType = embySubtitleFormat(deliveryURL, msi.GetCodec(), msi.GetMimeType())
			route.RouteSource = "delivery_url"
			route.DeliveryURLAccepted = true
			route.APIPrefixAdded = apiPrefixAdded
		}

		name := msi.GetDisplayTitle()
		if name == "" {
			if msi.GetTitle() != "" {
				name = msi.GetTitle()
			} else {
				name = msi.GetDisplayLanguage()
			}
		}

		subtitles = append(subtitles, &EmbySubtitleCache{
			URL:                 subtitleURLString,
			Type:                subtitleType,
			Name:                name,
			ContentType:         contentType,
			RouteSource:         route.RouteSource,
			DeliveryURLPresent:  route.DeliveryURLPresent,
			DeliveryURLAccepted: route.DeliveryURLAccepted,
			APIPrefixAdded:      route.APIPrefixAdded,
			FallbackAvailable:   route.FallbackAvailable,
			Cache: refreshcache0.NewRefreshCache(
				newEmbySubtitleCacheInitFunc(subtitleURLString, route), -1,
			),
		})
	}

	return subtitles
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
