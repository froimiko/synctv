package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"

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
	Host     string
	ServerID string
	APIKey   string
	UserID   string
	Backend  string
}

type embyItemGetter interface {
	GetItem(context.Context, *emby.GetItemReq) (*emby.Item, error)
}

const maxEmbyParentDepth = 128

var errEmbyItemOutsideRoot = errors.New("emby item is not in shared root")

func ValidateEmbyItemInRoot(
	ctx context.Context,
	cli embyItemGetter,
	host, token, userID, rootItemID, requestedItemID string,
) error {
	if cli == nil || userID == "" || rootItemID == "" || requestedItemID == "" {
		return errEmbyItemOutsideRoot
	}
	if requestedItemID == rootItemID {
		return nil
	}

	visited := make(map[string]struct{}, maxEmbyParentDepth)
	currentItemID := requestedItemID
	for depth := 0; depth < maxEmbyParentDepth; depth++ {
		if _, ok := visited[currentItemID]; ok {
			return errEmbyItemOutsideRoot
		}
		visited[currentItemID] = struct{}{}

		item, err := cli.GetItem(ctx, &emby.GetItemReq{
			Host:   host,
			Token:  token,
			UserId: userID,
			ItemId: currentItemID,
		})
		if err != nil || item == nil {
			return errEmbyItemOutsideRoot
		}
		if item.GetId() != currentItemID {
			return errEmbyItemOutsideRoot
		}

		parentItemID := item.GetParentId()
		if parentItemID == "" {
			return errEmbyItemOutsideRoot
		}
		if parentItemID == rootItemID {
			return nil
		}
		currentItemID = parentItemID
	}

	return errEmbyItemOutsideRoot
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
		Host:     v.Host,
		ServerID: v.ServerID,
		APIKey:   v.APIKey,
		UserID:   v.EmbyUserID,
		Backend:  v.Backend,
	}, nil
}

type EmbySource struct {
	URL         string
	Name        string
	Subtitles   []*EmbySubtitleCache
	IsTranscode bool
}

type EmbySubtitleCache struct {
	Cache *refreshcache0.RefreshCache[[]byte]
	URL   string
	Type  string
	Name  string
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

func NewEmbyMovieCacheInitFunc(
	movie *model.Movie,
	subPath string,
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

		if aucd.Host == "" || aucd.APIKey == "" {
			return nil, errors.New("not bind emby vendor")
		}

		cli := vendor.LoadEmbyClient(aucd.Backend)
		if err := ValidateEmbyItemInRoot(
			ctx, cli, aucd.Host, aucd.APIKey, aucd.UserID, rootItemID, requestedItemID,
		); err != nil {
			return nil, err
		}

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

	if movie.IsFolder && subPath == "" {
		return errors.New("sub path is empty")
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
	data, err := cli.PlaybackInfo(ctx, &emby.PlaybackInfoReq{
		Host:   aucd.Host,
		Token:  aucd.APIKey,
		UserId: aucd.UserID,
		ItemId: truePath,
	})
	if err != nil {
		return nil, fmt.Errorf("playback info: %w", err)
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

		u.Path = result
		query := url.Values{}
		query.Set("api_key", aucd.APIKey)
		query.Set("Static", "true")
		query.Set("MediaSourceId", v.GetId())
		u.RawQuery = query.Encode()
		source.URL = u.String()
	}

	return source, nil
}

func processEmbySubtitles(
	v *emby.MediaSourceInfo,
	truePath string,
	apiKey string,
	u *url.URL,
) []*EmbySubtitleCache {
	subtitles := make([]*EmbySubtitleCache, 0, len(v.GetMediaStreamInfo()))
	for _, msi := range v.GetMediaStreamInfo() {
		if msi.GetType() != "Subtitle" {
			continue
		}

		subtitleType := "srt"

		result, err := url.JoinPath(
			"emby",
			"Videos",
			truePath,
			v.GetId(),
			"Subtitles",
			strconv.FormatUint(msi.GetIndex(), 10),
			"Stream."+subtitleType,
		)
		if err != nil {
			continue
		}

		subtitleURL := *u
		subtitleURL.Path = result
		query := url.Values{}
		query.Set("api_key", apiKey)
		subtitleURL.RawQuery = query.Encode()
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
			URL:   subtitleURLString,
			Type:  subtitleType,
			Name:  name,
			Cache: refreshcache0.NewRefreshCache(newEmbySubtitleCacheInitFunc(subtitleURLString), -1),
		})
	}

	return subtitles
}

func newEmbySubtitleCacheInitFunc(url string) func(ctx context.Context) ([]byte, error) {
	return func(ctx context.Context) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, errors.New("failed to create subtitle request")
		}

		req.Header.Set("User-Agent", utils.UA)
		req.Header.Set("Referer", req.URL.Host)

		resp, err := uhc.Do(req)
		if err != nil {
			return nil, errors.New("failed to fetch subtitle")
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("unexpected subtitle status: %d", resp.StatusCode)
		}

		return io.ReadAll(resp.Body)
	}
}
