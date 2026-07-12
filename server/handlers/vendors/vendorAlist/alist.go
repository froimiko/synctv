package vendoralist

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	"github.com/synctv-org/synctv/internal/cache"
	"github.com/synctv-org/synctv/internal/db"
	dbModel "github.com/synctv-org/synctv/internal/model"
	"github.com/synctv-org/synctv/internal/op"
	"github.com/synctv-org/synctv/internal/vendor"
	"github.com/synctv-org/synctv/server/handlers/proxy"
	"github.com/synctv-org/synctv/server/middlewares"
	"github.com/synctv-org/synctv/server/model"
	"github.com/synctv-org/synctv/utils"
	"github.com/synctv-org/vendors/api/alist"
)

type AlistVendorService struct {
	room  *op.Room
	movie *op.Movie
}

func NewAlistVendorService(room *op.Room, movie *op.Movie) (*AlistVendorService, error) {
	if movie.VendorInfo.Vendor != dbModel.VendorAlist {
		return nil, fmt.Errorf("alist vendor not support vendor %s", movie.VendorInfo.Vendor)
	}

	return &AlistVendorService{
		room:  room,
		movie: movie,
	}, nil
}

func (s *AlistVendorService) Client() alist.AlistHTTPServer {
	return vendor.LoadAlistClient(s.movie.VendorInfo.Backend)
}

type dynamicMovieAlistCredentials struct {
	host    string
	token   string
	backend string
}

func dynamicMovieAlistCredentialsFromCache(
	ctx context.Context,
	creatorCache *cache.AlistUserCache,
	serverID string,
) (*dynamicMovieAlistCredentials, error) {
	aucd, err := creatorCache.LoadOrStore(ctx, serverID)
	if err != nil {
		return nil, err
	}

	return &dynamicMovieAlistCredentials{
		host:    aucd.Host,
		token:   aucd.Token,
		backend: aucd.Backend,
	}, nil
}

func loadDynamicMovieAlistCredentials(
	ctx context.Context,
	creatorID, serverID string,
) (*dynamicMovieAlistCredentials, error) {
	creator, err := op.LoadOrInitUserByID(creatorID)
	if err != nil {
		return nil, err
	}

	return dynamicMovieAlistCredentialsFromCache(ctx, creator.Value().AlistCache(), serverID)
}

//nolint:gosec
func (s *AlistVendorService) ListDynamicMovie(
	ctx context.Context,
	_ *op.User,
	subPath, keyword string,
	page, _max int,
) (*model.MovieList, error) {
	resp := &model.MovieList{
		Paths: []*model.MoviePath{},
	}

	serverID, truePath, err := s.movie.VendorInfo.Alist.ServerIDAndFilePath()
	if err != nil {
		return nil, fmt.Errorf("load alist server id error: %w", err)
	}

	truePath, err = cache.ResolveAlistPath(truePath, "")
	if err != nil {
		return nil, err
	}
	newPath, err := cache.ResolveAlistPath(truePath, subPath)
	if err != nil {
		return nil, err
	}

	credentials, err := loadDynamicMovieAlistCredentials(ctx, s.movie.CreatorID, serverID)
	if err != nil {
		if errors.Is(err, db.NotFoundError(db.ErrVendorNotFound)) {
			return nil, errors.New("alist server not found")
		}
		return nil, err
	}

	cli := vendor.LoadAlistClient(credentials.backend)
	if keyword != "" {
		data, err := cli.FsSearch(ctx, &alist.FsSearchReq{
			Token:    credentials.token,
			Password: s.movie.VendorInfo.Alist.Password,
			Parent:   newPath,
			Host:     credentials.host,
			Page:     uint64(page),
			PerPage:  uint64(_max),
			Keywords: keyword,
		})
		if err != nil {
			return nil, errors.New("failed to list media")
		}

		resp.Total = int64(data.GetTotal())

		resp.Movies = make([]*model.Movie, len(data.GetContent()))
		for i, flr := range data.GetContent() {
			fileSubPath, err := cache.ResolveAlistSearchSubPath(truePath, newPath, flr.GetParent(), flr.GetName())
			if err != nil {
				return nil, err
			}
			resultPath, err := cache.ResolveAlistPath(truePath, fileSubPath)
			if err != nil {
				return nil, err
			}

			resp.Movies[i] = &model.Movie{
				ID:        s.movie.ID,
				CreatedAt: s.movie.CreatedAt.UnixMilli(),
				Creator:   op.GetUserName(s.movie.CreatorID),
				CreatorID: s.movie.CreatorID,
				SubPath:   fileSubPath,
				Base: dbModel.MovieBase{
					Name:     flr.GetName(),
					IsFolder: flr.GetIsDir(),
					ParentID: dbModel.EmptyNullString(s.movie.ID),
					VendorInfo: dbModel.VendorInfo{
						Vendor:  dbModel.VendorAlist,
						Backend: credentials.backend,
						Alist: &dbModel.AlistStreamingInfo{
							Path: dbModel.FormatAlistPath(serverID, resultPath),
						},
					},
				},
			}
		}

		resp.Paths = model.GenDefaultSubPaths(s.movie.ID, subPath, true)

		return resp, nil
	}

	data, err := cli.FsList(ctx, &alist.FsListReq{
		Token:    credentials.token,
		Password: s.movie.VendorInfo.Alist.Password,
		Path:     newPath,
		Host:     credentials.host,
		Refresh:  false,
		Page:     uint64(page),
		PerPage:  uint64(_max),
	})
	if err != nil {
		return nil, errors.New("failed to list media")
	}

	resp.Total = int64(data.GetTotal())

	resp.Movies = make([]*model.Movie, len(data.GetContent()))
	for i, flr := range data.GetContent() {
		resp.Movies[i] = &model.Movie{
			ID:        s.movie.ID,
			CreatedAt: s.movie.CreatedAt.UnixMilli(),
			Creator:   op.GetUserName(s.movie.CreatorID),
			CreatorID: s.movie.CreatorID,
			SubPath:   "/" + strings.Trim(fmt.Sprintf("%s/%s", subPath, flr.GetName()), "/"),
			Base: dbModel.MovieBase{
				Name:     flr.GetName(),
				IsFolder: flr.GetIsDir(),
				ParentID: dbModel.EmptyNullString(s.movie.ID),
				VendorInfo: dbModel.VendorInfo{
					Vendor:  dbModel.VendorAlist,
					Backend: credentials.backend,
					Alist: &dbModel.AlistStreamingInfo{
						Path: dbModel.FormatAlistPath(serverID,
							"/"+strings.Trim(fmt.Sprintf("%s/%s", newPath, flr.GetName()), "/"),
						),
					},
				},
			},
		}
	}

	resp.Paths = model.GenDefaultSubPaths(s.movie.ID, subPath, true)

	return resp, nil
}

func (s *AlistVendorService) ProxyMovie(ctx *gin.Context) {
	log := middlewares.GetLogger(ctx)

	// Get cache data
	data, err := s.getCacheData(ctx)
	if err != nil {
		log.Error("proxy vendor movie error: failed to load media source")
		ctx.AbortWithStatusJSON(http.StatusInternalServerError, model.NewAPIErrorStringResp("failed to load media source"))
		return
	}

	// Handle different providers
	switch data.Provider {
	case cache.AlistProviderAli:
		s.handleAliProvider(ctx, log, data)
	case cache.AlistProvider115:
		fallthrough
	default:
		s.handleDefaultProvider(ctx, log, data)
	}
}

func (s *AlistVendorService) getCacheData(ctx *gin.Context) (*cache.AlistMovieCacheData, error) {
	u, err := op.LoadOrInitUserByID(s.movie.CreatorID)
	if err != nil {
		return nil, err
	}

	data, err := s.movie.AlistCache().Get(ctx, &cache.AlistMovieCacheFuncArgs{
		UserCache: u.Value().AlistCache(),
		UserAgent: utils.UA,
	})
	if err != nil {
		return nil, err
	}

	return data, nil
}

func (s *AlistVendorService) handleAliProvider(
	ctx *gin.Context,
	log *logrus.Entry,
	data *cache.AlistMovieCacheData,
) {
	switch ctx.Query("t") {
	case "":
		ali, err := data.Ali.Get(ctx)
		if err != nil {
			log.Error("proxy vendor movie error: failed to load media source")
			ctx.AbortWithStatusJSON(
				http.StatusInternalServerError,
				model.NewAPIErrorStringResp("failed to load media source"),
			)
			return
		}

		if !s.movie.Proxy {
			ctx.Data(http.StatusOK, "audio/mpegurl", ali.M3U8ListFile)
			return
		}

		if err := proxy.M3u8Data(
			ctx,
			ali.M3U8ListFile,
			"",
			ctx.GetString("token"),
			s.movie.RoomID,
			s.movie.ID,
		); err != nil {
			log.Error("proxy vendor movie error: failed to proxy media")
		}
	case "raw":
		ali, err := data.Ali.Get(ctx)
		if err != nil {
			log.Error("proxy vendor movie error: failed to load media source")
			ctx.AbortWithStatusJSON(
				http.StatusInternalServerError,
				model.NewAPIErrorStringResp("failed to load media source"),
			)
			return
		}

		if !s.movie.Proxy {
			ctx.AbortWithStatusJSON(
				http.StatusBadRequest,
				model.NewAPIErrorStringResp("proxy is not enabled"),
			)
			return
		}

		s.proxyURL(ctx, log, ali.URL)
	case "subtitle":
		s.handleAliSubtitle(ctx, log, data)
	default:
		ctx.AbortWithStatusJSON(
			http.StatusBadRequest,
			model.NewAPIErrorStringResp("invalid proxy type"),
		)
	}
}

func (s *AlistVendorService) handleDefaultProvider(
	ctx *gin.Context,
	log *logrus.Entry,
	data *cache.AlistMovieCacheData,
) {
	switch ctx.Query("t") {
	case "subtitle":
		idS := ctx.Query("id")
		if idS == "" {
			log.Error("proxy vendor subtitle error: id is empty")
			ctx.AbortWithStatusJSON(
				http.StatusBadRequest,
				model.NewAPIErrorStringResp("id is empty"),
			)
			return
		}

		id, err := strconv.Atoi(idS)
		if err != nil {
			log.Error("proxy vendor subtitle error: invalid id")
			ctx.AbortWithStatusJSON(
				http.StatusBadRequest,
				model.NewAPIErrorStringResp("invalid id"),
			)
			return
		}

		if id < 0 || id >= len(data.Subtitles) {
			log.Error("proxy vendor subtitle error: id out of range")
			ctx.AbortWithStatusJSON(
				http.StatusBadRequest,
				model.NewAPIErrorStringResp("id out of range"),
			)
			return
		}

		subtitle := data.Subtitles[id]
		b, err := subtitle.Cache.Get(ctx)
		if err != nil {
			log.Error("proxy vendor subtitle error: failed to load subtitle")
			ctx.AbortWithStatusJSON(
				http.StatusInternalServerError,
				model.NewAPIErrorStringResp("failed to load subtitle"),
			)
			return
		}

		http.ServeContent(ctx.Writer, ctx.Request, subtitle.Name, time.Now(), bytes.NewReader(b))
	default:
		if !s.movie.Proxy {
			log.Error("proxy vendor movie error: proxy is not enabled")
			ctx.AbortWithStatusJSON(
				http.StatusBadRequest,
				model.NewAPIErrorStringResp("proxy is not enabled"),
			)
			return
		}

		s.proxyURL(ctx, log, data.URL)
	}
}

func (s *AlistVendorService) proxyURL(ctx *gin.Context, log *logrus.Entry, url string) {
	if err := proxy.AutoProxyURL(
		ctx,
		url,
		s.movie.Type,
		nil,
		ctx.GetString("token"),
		s.movie.RoomID,
		s.movie.ID,
		proxy.WithProxyURLCache(true),
	); err != nil {
		log.Error("proxy vendor movie error: failed to proxy media")
	}
}

func (s *AlistVendorService) handleAliSubtitle(
	ctx *gin.Context,
	log *logrus.Entry,
	data *cache.AlistMovieCacheData,
) {
	idS := ctx.Query("id")
	if idS == "" {
		log.Error("proxy vendor subtitle error: id is empty")
		ctx.AbortWithStatusJSON(
			http.StatusBadRequest,
			model.NewAPIErrorStringResp("id is empty"),
		)
		return
	}

	id, err := strconv.Atoi(idS)
	if err != nil {
		log.Error("proxy vendor subtitle error: invalid id")
		ctx.AbortWithStatusJSON(
			http.StatusBadRequest,
			model.NewAPIErrorStringResp("invalid id"),
		)
		return
	}
	if id < 0 {
		log.Error("proxy vendor subtitle error: id out of range")
		ctx.AbortWithStatusJSON(
			http.StatusBadRequest,
			model.NewAPIErrorStringResp("id out of range"),
		)
		return
	}

	ali, err := data.Ali.Get(ctx)
	if err != nil {
		log.Error("proxy vendor subtitle error: failed to load subtitle")
		ctx.AbortWithStatusJSON(
			http.StatusInternalServerError,
			model.NewAPIErrorStringResp("failed to load subtitle"),
		)
		return
	}

	var subtitle *cache.AlistSubtitle
	switch {
	case id < len(data.Subtitles):
		subtitle = data.Subtitles[id]
	case id < len(data.Subtitles)+len(ali.Subtitles):
		subtitle = ali.Subtitles[id-len(data.Subtitles)]
	default:
		log.Error("proxy vendor subtitle error: id out of range")
		ctx.AbortWithStatusJSON(
			http.StatusBadRequest,
			model.NewAPIErrorStringResp("id out of range"),
		)
		return
	}

	b, err := subtitle.Cache.Get(ctx)
	if err != nil {
		log.Error("proxy vendor subtitle error: failed to load subtitle")
		ctx.AbortWithStatusJSON(
			http.StatusInternalServerError,
			model.NewAPIErrorStringResp("failed to load subtitle"),
		)
		return
	}

	http.ServeContent(ctx.Writer, ctx.Request, subtitle.Name, time.Now(), bytes.NewReader(b))
}

func (s *AlistVendorService) GenMovieInfo(
	ctx context.Context,
	user *op.User,
	userAgent, userToken string,
) (*dbModel.Movie, error) {
	if s.movie.Proxy {
		return s.GenProxyMovieInfo(ctx, user, userAgent, userToken)
	}

	movie := s.movie.Clone()

	var err error

	creator, err := op.LoadOrInitUserByID(movie.CreatorID)
	if err != nil {
		return nil, err
	}

	alistCache := s.movie.AlistCache()

	data, err := alistCache.Get(ctx, &cache.AlistMovieCacheFuncArgs{
		UserCache: creator.Value().AlistCache(),
		UserAgent: utils.UA,
	})
	if err != nil {
		return nil, errors.New("failed to load media source")
	}

	for i, subt := range data.Subtitles {
		if movie.Subtitles == nil {
			movie.Subtitles = make(map[string]*dbModel.Subtitle, len(data.Subtitles))
		}

		movie.Subtitles[subt.Name] = &dbModel.Subtitle{
			URL: fmt.Sprintf(
				"/api/room/movie/proxy/%s?t=subtitle&id=%d&token=%s&roomId=%s",
				movie.ID,
				i,
				userToken,
				movie.RoomID,
			),
			Type: subt.Type,
		}
	}

	switch data.Provider {
	case cache.AlistProviderAli:
		ali, err := data.Ali.Get(ctx)
		if err != nil {
			return nil, errors.New("failed to load media source")
		}

		movie.URL = fmt.Sprintf(
			"/api/room/movie/proxy/%s?token=%s&roomId=%s",
			movie.ID,
			userToken,
			movie.RoomID,
		)
		movie.Type = "m3u8"

		rawStreamURL := data.URL

		subPath := s.movie.SubPath()

		var rawType string
		if subPath == "" {
			rawType = utils.GetURLExtension(movie.VendorInfo.Alist.Path)
		} else {
			rawType = utils.GetURLExtension(subPath)
		}

		movie.MoreSources = []*dbModel.MoreSource{
			{
				Name: "raw",
				Type: rawType,
				URL:  rawStreamURL,
			},
		}

		for i, subt := range ali.Subtitles {
			if movie.Subtitles == nil {
				movie.Subtitles = make(map[string]*dbModel.Subtitle, len(data.Subtitles))
			}

			movie.Subtitles[subt.Name] = &dbModel.Subtitle{
				URL: fmt.Sprintf(
					"/api/room/movie/proxy/%s?t=subtitle&id=%d&token=%s&roomId=%s",
					movie.ID,
					len(data.Subtitles)+i,
					userToken,
					movie.RoomID,
				),
				Type: subt.Type,
			}
		}

	case cache.AlistProvider115:
		data, err = alistCache.GetRefreshFunc()(ctx, &cache.AlistMovieCacheFuncArgs{
			UserCache: creator.Value().AlistCache(),
			UserAgent: userAgent,
		})
		if err != nil {
			return nil, errors.New("failed to load media source")
		}

		movie.URL = data.URL

		movie.Subtitles = make(map[string]*dbModel.Subtitle, len(data.Subtitles))
		for _, subt := range data.Subtitles {
			movie.Subtitles[subt.Name] = &dbModel.Subtitle{
				URL:  subt.URL,
				Type: subt.Type,
			}
		}

	default:
		movie.URL = data.URL
	}

	movie.VendorInfo.Alist.Password = ""

	return movie, nil
}

func (s *AlistVendorService) GenProxyMovieInfo(
	ctx context.Context,
	_ *op.User,
	_, userToken string,
) (*dbModel.Movie, error) {
	movie := s.movie.Clone()

	var err error

	creator, err := op.LoadOrInitUserByID(movie.CreatorID)
	if err != nil {
		return nil, err
	}

	alistCache := s.movie.AlistCache()

	data, err := alistCache.Get(ctx, &cache.AlistMovieCacheFuncArgs{
		UserCache: creator.Value().AlistCache(),
		UserAgent: utils.UA,
	})
	if err != nil {
		return nil, errors.New("failed to load media source")
	}

	for i, subt := range data.Subtitles {
		if movie.Subtitles == nil {
			movie.Subtitles = make(map[string]*dbModel.Subtitle, len(data.Subtitles))
		}

		movie.Subtitles[subt.Name] = &dbModel.Subtitle{
			URL: fmt.Sprintf(
				"/api/room/movie/proxy/%s?t=subtitle&id=%d&token=%s&roomId=%s",
				movie.ID,
				i,
				userToken,
				movie.RoomID,
			),
			Type: subt.Type,
		}
	}

	switch data.Provider {
	case cache.AlistProviderAli:
		ali, err := data.Ali.Get(ctx)
		if err != nil {
			return nil, errors.New("failed to load media source")
		}

		movie.URL = fmt.Sprintf(
			"/api/room/movie/proxy/%s?token=%s&roomId=%s",
			movie.ID,
			userToken,
			movie.RoomID,
		)
		movie.Type = "m3u8"

		rawStreamURL := fmt.Sprintf(
			"/api/room/movie/proxy/%s?t=raw&token=%s&roomId=%s",
			movie.ID,
			userToken,
			movie.RoomID,
		)
		movie.MoreSources = []*dbModel.MoreSource{
			{
				Name: "raw",
				Type: utils.GetURLExtension(movie.VendorInfo.Alist.Path),
				URL:  rawStreamURL,
			},
		}

		for i, subt := range ali.Subtitles {
			if movie.Subtitles == nil {
				movie.Subtitles = make(map[string]*dbModel.Subtitle, len(data.Subtitles))
			}

			movie.Subtitles[subt.Name] = &dbModel.Subtitle{
				URL: fmt.Sprintf(
					"/api/room/movie/proxy/%s?t=subtitle&id=%d&token=%s&roomId=%s",
					movie.ID,
					len(data.Subtitles)+i,
					userToken,
					movie.RoomID,
				),
				Type: subt.Type,
			}
		}

	case cache.AlistProvider115:
		movie.URL = fmt.Sprintf(
			"/api/room/movie/proxy/%s?token=%s&roomId=%s",
			movie.ID,
			userToken,
			movie.RoomID,
		)
		movie.Type = utils.GetURLExtension(data.URL)

		// TODO: proxy subtitle

	default:
		movie.URL = fmt.Sprintf(
			"/api/room/movie/proxy/%s?token=%s&roomId=%s",
			movie.ID,
			userToken,
			movie.RoomID,
		)
		movie.Type = utils.GetURLExtension(data.URL)
	}

	movie.VendorInfo.Alist.Password = ""

	return movie, nil
}
