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
	log "github.com/sirupsen/logrus"
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

func embyAccessErrorCategory(err error) string {
	if errors.Is(err, db.ErrEmbyGrantDenied) || errors.Is(err, db.ErrEmbyGrantInternal) {
		return db.EmbyGrantErrorCategory(err)
	}
	return "internal_error"
}

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
	host             string
	apiKey           string
	userID           string
	backend          string
	bindingUpdatedAt time.Time
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
		host:             aucd.Host,
		apiKey:           aucd.APIKey,
		userID:           aucd.UserID,
		backend:          aucd.Backend,
		bindingUpdatedAt: aucd.BindingUpdatedAt,
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

func (s *EmbyVendorService) grantGeneration(
	serverID, rootItemID string,
	credentials *dynamicMovieEmbyCredentials,
) (string, error) {
	if s == nil || s.movie == nil || credentials == nil {
		return "", errors.New("invalid emby grant context")
	}
	return cache.EmbyGrantGeneration(
		s.movie.ID,
		s.movie.CreatorID,
		credentials.backend,
		serverID,
		rootItemID,
		credentials.userID,
		credentials.bindingUpdatedAt,
	)
}

func (s *EmbyVendorService) authorizeEmbyParent(
	serverID, rootItemID, parentItemID string,
	credentials *dynamicMovieEmbyCredentials,
	now time.Time,
) (string, error) {
	generation, err := s.grantGeneration(serverID, rootItemID, credentials)
	if err != nil {
		return "", db.NewEmbyGrantError("invalid_generation")
	}
	if err := cache.ValidateEmbyNavigationGrant(
		s.movie.ID, generation, rootItemID, parentItemID, true, now,
	); err != nil {
		return "", err
	}
	return generation, nil
}

func (s *EmbyVendorService) persistEmbyGrants(
	generation, parentItemID string,
	items []*emby.Item,
	now time.Time,
) error {
	grants, err := cache.NewEmbyRootGrants(s.movie.ID, generation, parentItemID, items, now)
	if err != nil {
		return db.NewEmbyGrantError("malformed_response")
	}
	if err := db.UpsertEmbyRootGrants(grants); err != nil {
		return db.NewEmbyGrantError("database_error")
	}
	return nil
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

	now := time.Now().UTC()
	generation, err := s.authorizeEmbyParent(
		serverID, rootItemID, requestedItemID, credentials, now,
	)
	if err != nil {
		if errors.Is(err, db.ErrEmbyGrantDenied) {
			log.WithField("category", embyAccessErrorCategory(err)).Warn("emby navigation authorization denied")
		} else {
			log.WithField("category", embyAccessErrorCategory(err)).Error("emby navigation authorization failed")
		}
		return nil, err
	}

	cli := vendor.LoadEmbyClient(credentials.backend)
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
		return nil, db.NewEmbyGrantError("upstream_error")
	}
	if data == nil {
		return nil, db.NewEmbyGrantError("nil_response")
	}
	if err := s.persistEmbyGrants(generation, requestedItemID, data.GetItems(), now); err != nil {
		log.WithField("category", embyAccessErrorCategory(err)).Error("emby navigation grant persistence failed")
		return nil, err
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
	return movie == nil || user == nil || movie.Proxy
}

func canRequestEmbyProxy(movie *op.Movie, user *op.User) bool {
	return movie != nil && user != nil && movie.Proxy
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

func loadAuthorizedEmbyMovieCacheData(
	ctx context.Context,
	movie *dbModel.Movie,
	subPath, creatorID string,
	now func() time.Time,
	loadCreatorCache func(context.Context, string) (*cache.EmbyUserCache, error),
	validate func(context.Context, *dbModel.Movie, string, *cache.EmbyUserCache, time.Time) error,
	get func(context.Context, *cache.EmbyUserCache) (*cache.EmbyMovieCacheData, error),
) (*cache.EmbyMovieCacheData, error) {
	creatorCache, err := loadCreatorCache(ctx, creatorID)
	if err != nil {
		return nil, err
	}
	if err := validate(ctx, movie, subPath, creatorCache, now()); err != nil {
		return nil, err
	}
	return get(ctx, creatorCache)
}

func (s *EmbyVendorService) embyMovieCacheData(ctx context.Context) (*cache.EmbyMovieCacheData, error) {
	return loadAuthorizedEmbyMovieCacheData(
		ctx,
		s.movie.Movie,
		s.movie.SubPath(),
		s.movie.CreatorID,
		func() time.Time { return time.Now().UTC() },
		func(_ context.Context, creatorID string) (*cache.EmbyUserCache, error) {
			creator, err := op.LoadOrInitUserByID(creatorID)
			if err != nil {
				return nil, err
			}
			return creator.Value().EmbyCache(), nil
		},
		cache.ValidateEmbyMovieGrant,
		func(ctx context.Context, creatorCache *cache.EmbyUserCache) (*cache.EmbyMovieCacheData, error) {
			return s.movie.EmbyCache().Get(ctx, creatorCache)
		},
	)
}

func writeEmbyAccessError(ctx *gin.Context, logger *log.Entry, err error, internalMessage string) {
	if errors.Is(err, db.ErrEmbyGrantDenied) {
		logger.WithField("category", embyAccessErrorCategory(err)).Warn("emby media authorization denied")
		ctx.AbortWithStatusJSON(http.StatusForbidden, model.NewAPIErrorResp(db.ErrEmbyGrantDenied))
		return
	}
	logger.WithField("category", embyAccessErrorCategory(err)).Error("emby media access failed")
	ctx.AbortWithStatusJSON(http.StatusInternalServerError, model.NewAPIErrorStringResp(internalMessage))
}

func (s *EmbyVendorService) handleProxyMovie(ctx *gin.Context) {
	logger := middlewares.GetLogger(ctx)

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
		logger.Error("proxy vendor movie invalid source")
		ctx.AbortWithStatusJSON(http.StatusBadRequest, model.NewAPIErrorStringResp("invalid source"))
		return
	}
	if source < 0 {
		logger.Error("proxy vendor movie source out of range")
		ctx.AbortWithStatusJSON(
			http.StatusBadRequest,
			model.NewAPIErrorStringResp("source out of range"),
		)
		return
	}

	embyC, err := s.embyMovieCacheData(ctx)
	if err != nil {
		writeEmbyAccessError(ctx, logger, err, "failed to load media source")
		return
	}

	if len(embyC.Sources) == 0 {
		logger.Error("proxy vendor movie has no source")
		ctx.AbortWithStatusJSON(http.StatusBadRequest, model.NewAPIErrorStringResp("no source"))
		return
	}

	if source >= len(embyC.Sources) {
		logger.Error("proxy vendor movie source out of range")
		ctx.AbortWithStatusJSON(
			http.StatusBadRequest,
			model.NewAPIErrorStringResp("source out of range"),
		)
		return
	}

	sourceCacheKey, err := embySourceCacheKey(embyC.Sources[source])
	if err != nil {
		logger.WithField("category", "internal_error").Error("proxy vendor movie source binding failed")
		ctx.AbortWithStatusJSON(http.StatusInternalServerError, model.NewAPIErrorStringResp("failed to load media source"))
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
		logger.WithField("category", "internal_error").Error("proxy vendor movie upstream proxy failed")
		ctx.AbortWithStatusJSON(
			http.StatusInternalServerError,
			model.NewAPIErrorStringResp("failed to load media source"),
		)
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

	embyC, err := s.embyMovieCacheData(ctx)
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
			logger := middlewares.GetLogger(ctx)
			if errors.Is(err, errInvalidSubtitleQuery) {
				ctx.AbortWithStatusJSON(http.StatusBadRequest, model.NewAPIErrorResp(err))
				return
			}
			writeEmbyAccessError(ctx, logger, err, "failed to load subtitle")
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
	if s == nil || s.movie == nil || user == nil {
		return nil, errors.New("invalid emby movie context")
	}
	if shouldProxyEmbyMovie(s.movie, user) {
		return s.GenProxyMovieInfo(ctx, user, userAgent, userToken)
	}

	movie := s.movie.Clone()

	data, err := s.embyMovieCacheData(ctx)
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
	user *op.User,
	_, userToken string,
) (*dbModel.Movie, error) {
	if s == nil || s.movie == nil || user == nil {
		return nil, errors.New("invalid emby movie context")
	}
	movie := s.movie.Clone()

	data, err := s.embyMovieCacheData(ctx)
	if err != nil {
		return nil, err
	}

	return rebuildEmbyProxyMovie(movie, data.Sources, userToken)
}
