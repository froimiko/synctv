package vendoremby

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	json "github.com/json-iterator/go"
	"github.com/synctv-org/synctv/internal/cache"
	"github.com/synctv-org/synctv/internal/db"
	dbModel "github.com/synctv-org/synctv/internal/model"
	"github.com/synctv-org/synctv/internal/vendor"
	"github.com/synctv-org/synctv/server/middlewares"
	"github.com/synctv-org/synctv/server/model"
	"github.com/synctv-org/vendors/api/emby"
)

type LoginReq struct {
	Host     string `json:"host"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func (r *LoginReq) Validate() error {
	if r.Host == "" {
		return errors.New("host is required")
	}

	url, err := url.Parse(r.Host)
	if err != nil {
		return err
	}

	if url.Scheme != "http" && url.Scheme != "https" {
		return errors.New("host is invalid")
	}

	r.Host = strings.TrimRight(url.String(), "/")
	if r.Username == "" {
		return errors.New("username is required")
	}

	return nil
}

func (r *LoginReq) Decode(ctx *gin.Context) error {
	return json.NewDecoder(ctx.Request.Body).Decode(r)
}

func validateEmbyLoginResponse(data *emby.LoginResp) error {
	if data == nil || data.GetServerId() == "" || data.GetToken() == "" || data.GetUserId() == "" {
		return errors.New("invalid emby login response")
	}
	return nil
}

func embyUserCacheDataFromVendor(v *dbModel.EmbyVendor) (*cache.EmbyUserCacheData, error) {
	if v == nil || v.Host == "" || v.ServerID == "" || v.APIKey == "" || v.EmbyUserID == "" || v.UpdatedAt.IsZero() {
		return nil, errors.New("invalid persisted emby vendor")
	}
	return &cache.EmbyUserCacheData{
		Host:             v.Host,
		ServerID:         v.ServerID,
		APIKey:           v.APIKey,
		Backend:          v.Backend,
		UserID:           v.EmbyUserID,
		BindingUpdatedAt: v.UpdatedAt,
	}, nil
}

func cachedEmbyLogoutData(userCache *cache.EmbyUserCache, serverID string) *cache.EmbyUserCacheData {
	if userCache == nil {
		return nil
	}
	cached, ok := userCache.LoadCache(serverID)
	if !ok {
		return nil
	}
	data, err := cached.Raw()
	if err != nil || data == nil {
		return nil
	}
	copied := *data
	return &copied
}

func deleteEmbyCachedBinding(userCache *cache.EmbyUserCache, serverID string) {
	if userCache != nil {
		userCache.Delete(serverID)
	}
}

func Login(ctx *gin.Context) {
	user := middlewares.GetUserEntry(ctx).Value()

	req := LoginReq{}
	if err := model.Decode(ctx, &req); err != nil {
		ctx.AbortWithStatusJSON(http.StatusBadRequest, model.NewAPIErrorResp(err))
		return
	}

	backend := ctx.Query("backend")
	cli := vendor.LoadEmbyClient(backend)

	data, err := cli.Login(ctx, &emby.LoginReq{
		Host:     req.Host,
		Username: req.Username,
		Password: req.Password,
	})
	if err != nil {
		ctx.AbortWithStatusJSON(http.StatusBadRequest, model.NewAPIErrorStringResp("emby login failed"))
		return
	}
	if err := validateEmbyLoginResponse(data); err != nil {
		ctx.AbortWithStatusJSON(
			http.StatusInternalServerError,
			model.NewAPIErrorStringResp("invalid emby login response"),
		)
		return
	}

	persisted, err := db.CreateOrSaveEmbyVendor(&dbModel.EmbyVendor{
		UserID:     user.ID,
		ServerID:   data.GetServerId(),
		Host:       req.Host,
		APIKey:     data.GetToken(),
		Backend:    backend,
		EmbyUserID: data.GetUserId(),
	})
	if err != nil {
		ctx.AbortWithStatusJSON(http.StatusInternalServerError, model.NewAPIErrorStringResp("internal server error"))
		return
	}

	cacheData, err := embyUserCacheDataFromVendor(persisted)
	if err != nil {
		deleteEmbyCachedBinding(user.EmbyCache(), persisted.ServerID)
		ctx.AbortWithStatusJSON(http.StatusInternalServerError, model.NewAPIErrorStringResp("internal server error"))
		return
	}
	_, err = user.EmbyCache().StoreOrRefreshWithDynamicFunc(
		ctx,
		persisted.ServerID,
		func(context.Context, string) (*cache.EmbyUserCacheData, error) {
			return cacheData, nil
		},
	)
	if err != nil {
		deleteEmbyCachedBinding(user.EmbyCache(), persisted.ServerID)
		ctx.AbortWithStatusJSON(http.StatusInternalServerError, model.NewAPIErrorStringResp("internal server error"))
		return
	}

	ctx.Status(http.StatusNoContent)
}

func Logout(ctx *gin.Context) {
	user := middlewares.GetUserEntry(ctx).Value()

	var req model.ServerIDReq
	if err := model.Decode(ctx, &req); err != nil {
		ctx.AbortWithStatusJSON(http.StatusBadRequest, model.NewAPIErrorResp(err))
		return
	}

	cached := cachedEmbyLogoutData(user.EmbyCache(), req.ServerID)
	if err := db.DeleteEmbyVendor(user.ID, req.ServerID); err != nil {
		ctx.AbortWithStatusJSON(http.StatusInternalServerError, model.NewAPIErrorStringResp("internal server error"))
		return
	}
	deleteEmbyCachedBinding(user.EmbyCache(), req.ServerID)
	if cached != nil {
		go logoutEmby(cached)
	}

	ctx.Status(http.StatusNoContent)
}

func logoutEmby(eucd *cache.EmbyUserCacheData) {
	if eucd == nil || eucd.APIKey == "" {
		return
	}

	_, _ = vendor.LoadEmbyClient(eucd.Backend).Logout(context.Background(), &emby.LogoutReq{
		Host:  eucd.Host,
		Token: eucd.APIKey,
	})
}
