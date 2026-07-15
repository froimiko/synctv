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
	Cache       *refreshcache0.RefreshCache[[]byte]
	URL         string
	Type        string
	Name        string
	ContentType string
}

type EmbyMovieCacheData struct {
	TranscodeSessionID string
	Sources            []EmbySource
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

		resp := &EmbyMovieCacheData{
			Sources:            make([]EmbySource, len(data.GetMediaSourceInfo())),
			TranscodeSessionID: data.GetPlaySessionID(),
		}

		u, err := url.Parse(aucd.Host)
		if err != nil {
			return nil, err
		}

		for i, v := range data.GetMediaSourceInfo() {
			source, err := processMediaSource(v, movie, aucd, requestedItemID, u)
			if err != nil {
				return nil, err
			}

			if source != nil {
				resp.Sources[i] = *source
				resp.Sources[i].Subtitles = processEmbySubtitles(v, requestedItemID, aucd.APIKey, u)
			}
		}
		return resp, nil
	}
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
		return nil, errors.New("invalid playback info context")
	}
	data, err := cli.PlaybackInfo(ctx, &emby.PlaybackInfoReq{
		Host:   aucd.Host,
		Token:  aucd.APIKey,
		UserId: aucd.UserID,
		ItemId: truePath,
	})
	if err != nil {
		return nil, errors.New("playback info request failed")
	}
	if data == nil {
		return nil, errors.New("playback info response is empty")
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

type embySubtitleFormat struct {
	Type        string
	ContentType string
}

var embySubtitleFormats = map[string]embySubtitleFormat{
	"srt": {Type: "srt", ContentType: "application/x-subrip; charset=utf-8"},
	"vtt": {Type: "vtt", ContentType: "text/vtt; charset=utf-8"},
	"ass": {Type: "ass", ContentType: "text/x-ssa; charset=utf-8"},
}

func normalizeEmbySubtitleCodec(codec string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "srt", "subrip":
		return "srt", true
	case "vtt", "webvtt":
		return "vtt", true
	case "ass", "ssa":
		return "ass", true
	default:
		return "", false
	}
}

func normalizeEmbySubtitleMIME(raw string) (string, bool) {
	mediaType, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return "", false
	}
	switch strings.ToLower(mediaType) {
	case "application/x-subrip", "application/srt", "text/srt", "text/x-srt":
		return "srt", true
	case "text/vtt", "application/vtt", "text/webvtt":
		return "vtt", true
	case "text/x-ssa", "text/ssa", "application/x-ssa", "application/ssa":
		return "ass", true
	case "text/x-ass", "text/ass", "application/x-ass", "application/ass":
		return "ass", true
	default:
		return "", false
	}
}

func normalizeEmbySubtitleExtension(rawPath string) (string, bool) {
	ext := strings.TrimPrefix(strings.ToLower(path.Ext(rawPath)), ".")
	if ext == "" {
		return "", false
	}
	return normalizeEmbySubtitleCodec(ext)
}

func embySubtitleStreamFormat(msi *emby.MediaStreamInfo, deliveryURL *url.URL) (embySubtitleFormat, bool) {
	if msi == nil || msi.GetType() != "Subtitle" || !msi.GetIsTextSubtitleStream() {
		return embySubtitleFormat{}, false
	}

	var evidence []string
	if codec := strings.TrimSpace(msi.GetCodec()); codec != "" {
		format, ok := normalizeEmbySubtitleCodec(codec)
		if !ok {
			return embySubtitleFormat{}, false
		}
		evidence = append(evidence, format)
	}
	if mimeType := strings.TrimSpace(msi.GetMimeType()); mimeType != "" {
		format, ok := normalizeEmbySubtitleMIME(mimeType)
		if !ok {
			return embySubtitleFormat{}, false
		}
		evidence = append(evidence, format)
	}
	if deliveryURL != nil {
		if ext := path.Ext(deliveryURL.Path); ext != "" {
			format, ok := normalizeEmbySubtitleExtension(deliveryURL.Path)
			if !ok {
				return embySubtitleFormat{}, false
			}
			evidence = append(evidence, format)
		}
	}
	if len(evidence) == 0 {
		return embySubtitleFormat{}, false
	}
	for _, candidate := range evidence[1:] {
		if candidate != evidence[0] {
			return embySubtitleFormat{}, false
		}
	}
	format, ok := embySubtitleFormats[evidence[0]]
	return format, ok
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

func sameEmbyOrigin(a, b *url.URL) bool {
	if a == nil || b == nil ||
		!strings.EqualFold(a.Scheme, b.Scheme) ||
		!strings.EqualFold(a.Hostname(), b.Hostname()) {
		return false
	}
	aPort, aOK := embyEffectivePort(a)
	bPort, bOK := embyEffectivePort(b)
	return aOK && bOK && aPort == bPort
}

func validEmbySubtitleBaseURL(base *url.URL) bool {
	if base == nil || base.User != nil || base.Fragment != "" || base.Hostname() == "" ||
		!strings.EqualFold(base.Scheme, "http") && !strings.EqualFold(base.Scheme, "https") {
		return false
	}
	_, ok := embyEffectivePort(base)
	return ok
}

func resolveEmbySubtitleDeliveryURL(base *url.URL, raw string) (*url.URL, url.Values, bool) {
	if !validEmbySubtitleBaseURL(base) {
		return nil, nil, false
	}

	reference, err := url.Parse(raw)
	if err != nil || reference.User != nil || reference.Fragment != "" {
		return nil, nil, false
	}
	if reference.RawQuery != "" {
		if _, err := url.ParseQuery(reference.RawQuery); err != nil {
			return nil, nil, false
		}
	}
	if reference.IsAbs() {
		if !strings.EqualFold(reference.Scheme, "http") && !strings.EqualFold(reference.Scheme, "https") ||
			reference.Hostname() == "" || !sameEmbyOrigin(base, reference) {
			return nil, nil, false
		}
	} else if reference.Host != "" {
		return nil, nil, false
	}

	resolved := base.ResolveReference(reference)
	if resolved.User != nil || resolved.Fragment != "" || !sameEmbyOrigin(base, resolved) {
		return nil, nil, false
	}
	query := url.Values{}
	if resolved.RawQuery != "" {
		query, err = url.ParseQuery(resolved.RawQuery)
		if err != nil {
			return nil, nil, false
		}
	}
	resolved.Scheme = strings.ToLower(resolved.Scheme)
	return resolved, query, true
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
		var (
			deliveryURL   *url.URL
			deliveryQuery url.Values
		)
		if rawDeliveryURL := strings.TrimSpace(msi.GetDeliveryUrl()); rawDeliveryURL != "" {
			var ok bool
			deliveryURL, deliveryQuery, ok = resolveEmbySubtitleDeliveryURL(base, rawDeliveryURL)
			if !ok {
				continue
			}
		}

		format, ok := embySubtitleStreamFormat(msi, deliveryURL)
		if !ok {
			continue
		}

		subtitleURL := deliveryURL
		if subtitleURL == nil {
			subtitleURL, ok = embySubtitleFallbackURL(base, truePath, v.GetId(), msi.GetIndex(), apiKey)
			if !ok {
				continue
			}
			format = embySubtitleFormats["vtt"]
		} else {
			setCanonicalEmbyAPIKey(subtitleURL, deliveryQuery, apiKey)
		}
		subtitleURLString := subtitleURL.String()

		name := msi.GetDisplayTitle()
		if name == "" {
			if msi.GetTitle() != "" {
				name = msi.GetTitle()
			} else {
				name = msi.GetDisplayLanguage()
			}
		}

		subtitles = append(subtitles, &EmbySubtitleCache{
			URL:         subtitleURLString,
			Type:        format.Type,
			Name:        name,
			ContentType: format.ContentType,
			Cache:       refreshcache0.NewRefreshCache(newEmbySubtitleCacheInitFunc(subtitleURLString), -1),
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

func newEmbySubtitleCacheInitFunc(rawURL string) func(ctx context.Context) ([]byte, error) {
	client := newEmbySubtitleHTTPClient()
	return func(ctx context.Context) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, errors.New("failed to create subtitle request")
		}

		req.Header.Set("User-Agent", utils.UA)
		req.Header.Set("Referer", req.URL.Scheme+"://"+req.URL.Host)

		resp, err := client.Do(req)
		if err != nil {
			return nil, errors.New("failed to fetch subtitle")
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("unexpected subtitle status: %d", resp.StatusCode)
		}
		if resp.ContentLength > subtitleMaxLength {
			return nil, errors.New("subtitle response too large")
		}

		data, err := io.ReadAll(io.LimitReader(resp.Body, subtitleMaxLength+1))
		if err != nil {
			return nil, errors.New("failed to read subtitle")
		}
		if len(data) > subtitleMaxLength {
			return nil, errors.New("subtitle response too large")
		}
		return data, nil
	}
}
