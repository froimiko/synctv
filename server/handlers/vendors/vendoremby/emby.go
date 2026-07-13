package vendoremby

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/synctv-org/synctv/internal/cache"
	"github.com/synctv-org/synctv/internal/db"
	dbModel "github.com/synctv-org/synctv/internal/model"
	"github.com/synctv-org/synctv/internal/op"
	"github.com/synctv-org/synctv/internal/vendor"
	"github.com/synctv-org/synctv/server/handlers/proxy"
	"github.com/synctv-org/synctv/server/middlewares"
	"github.com/synctv-org/synctv/server/model"
	"github.com/synctv-org/synctv/utils"
	"github.com/synctv-org/vendors/api/emby"
)

var errInvalidSubtitleQuery = errors.New("invalid subtitle query")

type EmbyVendorService struct {
	room  *op.Room
	movie *op.Movie
}

func NewEmbyVendorService(room *op.Room, movie *op.Movie) (*EmbyVendorService, error) {
	if movie.VendorInfo.Vendor != dbModel.VendorEmby {
		return nil, fmt.Errorf("emby vendor not support vendor %s", movie.VendorInfo.Vendor)
	}

	return &EmbyVendorService{
		room:  room,
		movie: movie,
	}, nil
}

func (s *EmbyVendorService) Client() emby.EmbyHTTPServer {
	return vendor.LoadEmbyClient(s.movie.VendorInfo.Backend)
}

type dynamicMovieEmbyCredentials struct {
	host    string
	apiKey  string
	userID  string
	backend string
}

func dynamicMovieEmbyCredentialsFromCache(
	ctx context.Context,
	creatorCache *cache.EmbyUserCache,
	serverID string,
) (*dynamicMovieEmbyCredentials, error) {
	aucd, err := creatorCache.LoadOrStore(ctx, serverID)
	if err != nil {
		return nil, err
	}

	return &dynamicMovieEmbyCredentials{
		host:    aucd.Host,
		apiKey:  aucd.APIKey,
		userID:  aucd.UserID,
		backend: aucd.Backend,
	}, nil
}

func loadDynamicMovieEmbyCredentials(
	ctx context.Context,
	creatorID, serverID string,
) (*dynamicMovieEmbyCredentials, error) {
	creator, err := op.LoadOrInitUserByID(creatorID)
	if err != nil {
		return nil, err
	}

	return dynamicMovieEmbyCredentialsFromCache(ctx, creator.Value().EmbyCache(), serverID)
}

//nolint:gosec
func (s *EmbyVendorService) ListDynamicMovie(
	ctx context.Context,
	_ *op.User,
	subPath, keyword string,
	page, _max int,
) (*model.MovieList, error) {
	resp := &model.MovieList{
		Paths: []*model.MoviePath{},
	}

	serverID, rootItemID, err := s.movie.VendorInfo.Emby.ServerIDAndFilePath()
	if err != nil {
		return nil, fmt.Errorf("load emby server id error: %w", err)
	}

	requestedItemID := rootItemID
	if subPath != "" {
		requestedItemID = subPath
	}

	credentials, err := loadDynamicMovieEmbyCredentials(ctx, s.movie.CreatorID, serverID)
	if err != nil {
		if errors.Is(err, db.NotFoundError(db.ErrVendorNotFound)) {
			return nil, errors.New("emby server not found")
		}
		return nil, err
	}

	cli := vendor.LoadEmbyClient(credentials.backend)
	if err := cache.ValidateEmbyItemInRoot(
		ctx, cli, credentials.host, credentials.apiKey, credentials.userID, rootItemID, requestedItemID,
	); err != nil {
		return nil, err
	}

	data, err := cli.FsList(ctx, &emby.FsListReq{
		Host:       credentials.host,
		Path:       requestedItemID,
		Token:      credentials.apiKey,
		UserId:     credentials.userID,
		Limit:      uint64(_max),
		StartIndex: uint64((page - 1) * _max),
		SearchTerm: keyword,
	})
	if err != nil {
		return nil, fmt.Errorf("emby fs list error: %w", err)
	}

	resp.Total = int64(data.GetTotal())

	resp.Movies = make([]*model.Movie, len(data.GetItems()))
	for i, flr := range data.GetItems() {
		resp.Movies[i] = &model.Movie{
			ID:        s.movie.ID,
			CreatedAt: s.movie.CreatedAt.UnixMilli(),
			Creator:   op.GetUserName(s.movie.CreatorID),
			CreatorID: s.movie.CreatorID,
			SubPath:   flr.GetId(),
			Base: dbModel.MovieBase{
				Name:     flr.GetName(),
				IsFolder: flr.GetIsFolder(),
				ParentID: dbModel.EmptyNullString(s.movie.ID),
				VendorInfo: dbModel.VendorInfo{
					Vendor:  dbModel.VendorEmby,
					Backend: credentials.backend,
					Emby: &dbModel.EmbyStreamingInfo{
						Path: dbModel.FormatEmbyPath(serverID, flr.GetId()),
					},
				},
			},
		}
	}

	return resp, nil
}

func shouldProxyEmbyMovie(movie *op.Movie, user *op.User) bool {
	return movie == nil || user == nil || movie.Proxy || user.ID != movie.CreatorID
}

func canRequestEmbyProxy(movie *op.Movie, user *op.User) bool {
	return movie != nil && user != nil && (movie.Proxy || user.ID != movie.CreatorID)
}

func embySourceCacheKey(source cache.EmbySource) (string, error) {
	sourceCacheKey, err := url.Parse(source.URL)
	if err != nil {
		return "", errors.New("invalid media source")
	}

	query := sourceCacheKey.Query()
	query.Del("DeviceId")
	query.Del("PlaySessionId")
	sourceCacheKey.RawQuery = query.Encode()
	return sourceCacheKey.String(), nil
}

func (s *EmbyVendorService) handleProxyMovie(ctx *gin.Context) {
	log := middlewares.GetLogger(ctx)

	userEntryValue, ok := ctx.Get("user")
	if !ok {
		ctx.AbortWithStatusJSON(http.StatusForbidden, model.NewAPIErrorStringResp("proxy access denied"))
		return
	}
	userEntry, ok := userEntryValue.(*op.UserEntry)
	if !ok || userEntry == nil || !canRequestEmbyProxy(s.movie, userEntry.Value()) {
		ctx.AbortWithStatusJSON(http.StatusForbidden, model.NewAPIErrorStringResp("proxy access denied"))
		return
	}

	source, err := strconv.Atoi(ctx.Query("source"))
	if err != nil {
		log.Errorf("proxy vendor movie error: invalid source")
		ctx.AbortWithStatusJSON(http.StatusBadRequest, model.NewAPIErrorStringResp("invalid source"))
		return
	}
	if source < 0 {
		log.Errorf("proxy vendor movie error: %v", "source out of range")
		ctx.AbortWithStatusJSON(
			http.StatusBadRequest,
			model.NewAPIErrorStringResp("source out of range"),
		)
		return
	}

	u, err := op.LoadOrInitUserByID(s.movie.CreatorID)
	if err != nil {
		log.Errorf("proxy vendor movie error: %v", err)
		ctx.AbortWithStatusJSON(http.StatusBadRequest, model.NewAPIErrorStringResp("failed to load media source"))
		return
	}

	embyC, err := s.movie.EmbyCache().Get(ctx, u.Value().EmbyCache())
	if err != nil {
		log.Errorf("proxy vendor movie error: %v", err)
		ctx.AbortWithStatusJSON(http.StatusBadRequest, model.NewAPIErrorStringResp("failed to load media source"))
		return
	}

	if len(embyC.Sources) == 0 {
		log.Errorf("proxy vendor movie error: %v", "no source")
		ctx.AbortWithStatusJSON(http.StatusBadRequest, model.NewAPIErrorStringResp("no source"))
		return
	}

	if source >= len(embyC.Sources) {
		log.Errorf("proxy vendor movie error: %v", "source out of range")
		ctx.AbortWithStatusJSON(
			http.StatusBadRequest,
			model.NewAPIErrorStringResp("source out of range"),
		)
		return
	}

	sourceCacheKey, err := embySourceCacheKey(embyC.Sources[source])
	if err != nil {
		log.Errorf("proxy vendor movie error: invalid media source")
		ctx.AbortWithStatusJSON(http.StatusBadRequest, model.NewAPIErrorStringResp("invalid media source"))
		return
	}

	err = proxy.AutoProxyURL(ctx,
		embyC.Sources[source].URL,
		"",
		nil,
		ctx.GetString("token"),
		s.movie.RoomID,
		s.movie.ID,
		proxy.WithProxyURLCache(true),
		proxy.WithProxyURLCacheKey(sourceCacheKey),
	)
	if err != nil {
		log.Errorf("proxy vendor movie error: %v", err)
	}
}

func (s *EmbyVendorService) handleSubtitle(ctx *gin.Context) error {
	source, err := strconv.Atoi(ctx.Query("source"))
	if err != nil {
		return fmt.Errorf("%w: invalid source", errInvalidSubtitleQuery)
	}
	if source < 0 {
		return fmt.Errorf("%w: source out of range", errInvalidSubtitleQuery)
	}

	id, err := strconv.Atoi(ctx.Query("id"))
	if err != nil {
		return fmt.Errorf("%w: invalid id", errInvalidSubtitleQuery)
	}
	if id < 0 {
		return fmt.Errorf("%w: id out of range", errInvalidSubtitleQuery)
	}

	u, err := op.LoadOrInitUserByID(s.movie.CreatorID)
	if err != nil {
		return err
	}

	embyC, err := s.movie.EmbyCache().Get(ctx, u.Value().EmbyCache())
	if err != nil {
		return err
	}

	if source >= len(embyC.Sources) {
		return fmt.Errorf("%w: source out of range", errInvalidSubtitleQuery)
	}

	if id >= len(embyC.Sources[source].Subtitles) {
		return fmt.Errorf("%w: id out of range", errInvalidSubtitleQuery)
	}

	data, err := embyC.Sources[source].Subtitles[id].Cache.Get(ctx)
	if err != nil {
		return err
	}

	http.ServeContent(
		ctx.Writer,
		ctx.Request,
		embyC.Sources[source].Subtitles[id].Name,
		time.Now(),
		bytes.NewReader(data),
	)

	return nil
}

func (s *EmbyVendorService) ProxyMovie(ctx *gin.Context) {
	switch t := ctx.Query("t"); t {
	case "":
		s.handleProxyMovie(ctx)
	case "subtitle":
		if err := s.handleSubtitle(ctx); err != nil {
			log := middlewares.GetLogger(ctx)
			log.Errorf("proxy vendor subtitle error: %v", err)

			if errors.Is(err, errInvalidSubtitleQuery) {
				ctx.AbortWithStatusJSON(http.StatusBadRequest, model.NewAPIErrorResp(err))
				return
			}

			ctx.AbortWithStatusJSON(
				http.StatusInternalServerError,
				model.NewAPIErrorStringResp("failed to load subtitle"),
			)
		}
	default:
		ctx.AbortWithStatusJSON(
			http.StatusBadRequest,
			model.NewAPIErrorStringResp("unknown proxy type: "+t),
		)
	}
}

func embyMovieProxyURL(movieID, roomID, userToken string, source int) (string, error) {
	rawPath, err := url.JoinPath("/api/room/movie/proxy", movieID)
	if err != nil {
		return "", err
	}

	rawQuery := url.Values{}
	rawQuery.Set("source", strconv.Itoa(source))
	rawQuery.Set("token", userToken)
	rawQuery.Set("roomId", roomID)
	return (&url.URL{Path: rawPath, RawQuery: rawQuery.Encode()}).String(), nil
}

func embySubtitleProxyURL(movieID, roomID, userToken string, source, id int) (string, error) {
	rawPath, err := url.JoinPath("/api/room/movie/proxy", movieID)
	if err != nil {
		return "", err
	}

	rawQuery := url.Values{}
	rawQuery.Set("t", "subtitle")
	rawQuery.Set("source", strconv.Itoa(source))
	rawQuery.Set("id", strconv.Itoa(id))
	rawQuery.Set("token", userToken)
	rawQuery.Set("roomId", roomID)

	return (&url.URL{Path: rawPath, RawQuery: rawQuery.Encode()}).String(), nil
}

func (s *EmbyVendorService) GenMovieInfo(
	ctx context.Context,
	user *op.User,
	userAgent, userToken string,
) (*dbModel.Movie, error) {
	if shouldProxyEmbyMovie(s.movie, user) {
		return s.GenProxyMovieInfo(ctx, user, userAgent, userToken)
	}

	movie := s.movie.Clone()

	var err error

	u, err := op.LoadOrInitUserByID(movie.CreatorID)
	if err != nil {
		return nil, err
	}

	data, err := s.movie.EmbyCache().Get(ctx, u.Value().EmbyCache())
	if err != nil {
		return nil, err
	}

	hasSource := false
	for sourceIndex, source := range data.Sources {
		if source.URL == "" {
			continue
		}

		if !hasSource {
			movie.URL = source.URL
			hasSource = true
		} else {
			movie.MoreSources = append(movie.MoreSources,
				&dbModel.MoreSource{
					Name: source.Name,
					URL:  source.URL,
				},
			)
		}

		for subtitleIndex, subtitle := range source.Subtitles {
			if movie.Subtitles == nil {
				movie.Subtitles = make(map[string]*dbModel.Subtitle, len(source.Subtitles))
			}

			subtitleURL, err := embySubtitleProxyURL(
				movie.ID,
				movie.RoomID,
				userToken,
				sourceIndex,
				subtitleIndex,
			)
			if err != nil {
				return nil, err
			}

			movie.Subtitles[subtitle.Name] = &dbModel.Subtitle{
				URL:  subtitleURL,
				Type: subtitle.Type,
			}
		}
	}

	if !hasSource {
		return nil, errors.New("no source")
	}

	return movie, nil
}

func rebuildEmbyProxyMovie(
	movie *dbModel.Movie,
	sources []cache.EmbySource,
	userToken string,
) (*dbModel.Movie, error) {
	if movie == nil {
		return nil, errors.New("movie is required")
	}

	movie.URL = ""
	movie.MoreSources = nil
	movie.Headers = nil
	movie.Subtitles = nil

	hasSource := false
	for sourceIndex, source := range sources {
		if source.URL == "" {
			continue
		}

		proxyURL, err := embyMovieProxyURL(movie.ID, movie.RoomID, userToken, sourceIndex)
		if err != nil {
			return nil, err
		}

		sourceType := utils.GetURLExtension(source.URL)
		if !hasSource {
			movie.URL = proxyURL
			movie.Type = sourceType
			hasSource = true
		} else {
			movie.MoreSources = append(movie.MoreSources, &dbModel.MoreSource{
				Name: source.Name,
				URL:  proxyURL,
				Type: sourceType,
			})
		}

		for subtitleIndex, subtitle := range source.Subtitles {
			if movie.Subtitles == nil {
				movie.Subtitles = make(map[string]*dbModel.Subtitle, len(source.Subtitles))
			}

			subtitleURL, err := embySubtitleProxyURL(
				movie.ID,
				movie.RoomID,
				userToken,
				sourceIndex,
				subtitleIndex,
			)
			if err != nil {
				return nil, err
			}

			movie.Subtitles[subtitle.Name] = &dbModel.Subtitle{
				URL:  subtitleURL,
				Type: subtitle.Type,
			}
		}
	}

	if !hasSource {
		return nil, errors.New("no source")
	}

	return movie, nil
}

func (s *EmbyVendorService) GenProxyMovieInfo(
	ctx context.Context,
	_ *op.User,
	_, userToken string,
) (*dbModel.Movie, error) {
	movie := s.movie.Clone()

	u, err := op.LoadOrInitUserByID(movie.CreatorID)
	if err != nil {
		return nil, err
	}

	data, err := s.movie.EmbyCache().Get(ctx, u.Value().EmbyCache())
	if err != nil {
		return nil, errors.New("failed to load media source")
	}

	return rebuildEmbyProxyMovie(movie, data.Sources, userToken)
}
