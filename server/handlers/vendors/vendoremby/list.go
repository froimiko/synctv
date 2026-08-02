package vendoremby

import (
	"context"
	"errors"
	"fmt"
	log "github.com/sirupsen/logrus"
	"github.com/synctv-org/synctv/internal/cache"
	"net/http"

	"github.com/gin-gonic/gin"
	json "github.com/json-iterator/go"
	"github.com/synctv-org/synctv/internal/db"
	dbModel "github.com/synctv-org/synctv/internal/model"
	"github.com/synctv-org/synctv/internal/vendor"
	"github.com/synctv-org/synctv/server/middlewares"
	"github.com/synctv-org/synctv/server/model"
	"github.com/synctv-org/synctv/utils"
	"github.com/synctv-org/vendors/api/emby"
	"gorm.io/gorm"
)

type ListReq struct {
	Path    string `json:"path"`
	Keyword string `json:"keyword"`
}

func (r *ListReq) Validate() (err error) {
	return nil
}

func (r *ListReq) Decode(ctx *gin.Context) error {
	return json.NewDecoder(ctx.Request.Body).Decode(r)
}

type EmbyFileItem struct {
	*model.Item
	Type string `json:"type"`
}

type EmbyFSListResp = model.VendorFSListResp[*EmbyFileItem]

//nolint:gosec
func List(ctx *gin.Context) {
	user := middlewares.GetUserEntry(ctx).Value()

	req := ListReq{}
	if err := model.Decode(ctx, &req); err != nil {
		ctx.AbortWithStatusJSON(http.StatusBadRequest, model.NewAPIErrorResp(err))
		return
	}

	page, size, err := utils.GetPageAndMax(ctx)
	if err != nil {
		ctx.AbortWithStatusJSON(http.StatusBadRequest, model.NewAPIErrorResp(err))
		return
	}

	if req.Path == "" {
		if req.Keyword != "" {
			ctx.AbortWithStatusJSON(
				http.StatusBadRequest,
				model.NewAPIErrorStringResp(
					"keywords is not supported when not choose server (server id is empty)",
				),
			)

			return
		}

		socpes := [](func(*gorm.DB) *gorm.DB){
			db.OrderByCreatedAtAsc,
		}

		total, err := db.GetEmbyVendorsCount(user.ID, socpes...)
		if err != nil {
			ctx.AbortWithStatusJSON(http.StatusInternalServerError, model.NewAPIErrorResp(err))
			return
		}

		if total == 0 {
			ctx.JSON(http.StatusBadRequest, model.NewAPIErrorStringResp("emby server not found"))
			return
		}

		ev, err := db.GetEmbyVendors(user.ID, append(socpes, db.Paginate(page, size))...)
		if err != nil {
			if errors.Is(err, db.NotFoundError(db.ErrVendorNotFound)) {
				ctx.JSON(
					http.StatusBadRequest,
					model.NewAPIErrorStringResp("emby server not found"),
				)

				return
			}

			ctx.AbortWithStatusJSON(http.StatusInternalServerError, model.NewAPIErrorResp(err))

			return
		}

		if total == 1 {
			req.Path = ev[0].ServerID + "/"
			goto EmbyFSListResp
		}

		resp := EmbyFSListResp{
			Paths: []*model.Path{
				{
					Name: "",
					Path: "",
				},
			},
			Total: uint64(total),
		}

		for _, evi := range ev {
			resp.Items = append(resp.Items, &EmbyFileItem{
				Item: &model.Item{
					Name:  evi.Host,
					Path:  evi.ServerID + `/`,
					IsDir: true,
				},
				Type: "server",
			})
		}

		ctx.JSON(http.StatusOK, model.NewAPIDataResp(resp))

		return
	}

EmbyFSListResp:

	var serverID string

	serverID, req.Path, err = dbModel.GetEmbyServerIDFromPath(req.Path)
	if err != nil {
		ctx.AbortWithStatusJSON(http.StatusBadRequest, model.NewAPIErrorResp(err))
		return
	}

	aucd, err := user.EmbyCache().LoadOrStore(ctx, serverID)
	if err != nil {
		if errors.Is(err, db.NotFoundError(db.ErrVendorNotFound)) {
			ctx.JSON(http.StatusBadRequest, model.NewAPIErrorStringResp("emby server not found"))
			return
		}

		ctx.AbortWithStatusJSON(http.StatusInternalServerError, model.NewAPIErrorResp(err))

		return
	}

	cli := vendor.LoadEmbyClient(ctx.Query("backend"))

	data, err := withEmbyTokenRefresh(
		ctx,
		func(refreshCtx context.Context) (*cache.EmbyUserCacheData, error) {
			return refreshEmbyToken(refreshCtx, user, serverID)
		},
		func(callCtx context.Context, binding *cache.EmbyUserCacheData) (*emby.FsListResp, error) {
			return cli.FsList(callCtx, &emby.FsListReq{
				Host:       binding.Host,
				Path:       req.Path,
				Token:      binding.APIKey,
				UserId:     binding.UserID,
				Limit:      uint64(size),
				StartIndex: uint64((page - 1) * size),
				SearchTerm: req.Keyword,
			})
		},
		aucd,
	)
	if err != nil {
		// A token that is still rejected after a refresh means the stored
		// credentials can no longer authenticate; tell the user to rebind rather
		// than leaking the upstream 401 body.
		if isEmbyUnauthorized(err) {
			log.WithField("error", err.Error()).Error("emby fs list unauthorized after refresh")
			ctx.AbortWithStatusJSON(
				http.StatusUnauthorized,
				model.NewAPIErrorStringResp("emby login expired, please rebind"),
			)

			return
		}

		ctx.AbortWithStatusJSON(
			http.StatusInternalServerError,
			model.NewAPIErrorResp(fmt.Errorf("emby fs list error: %w", err)),
		)

		return
	}

	resp := EmbyFSListResp{
		Paths: []*model.Path{
			{},
		},
	}
	for _, p := range data.GetPaths() {
		n := p.GetName()
		if p.GetPath() == "1" {
			n = aucd.Host
		}

		resp.Paths = append(resp.Paths, &model.Path{
			Name: n,
			Path: fmt.Sprintf("%s/%s", aucd.ServerID, p.GetPath()),
		})
	}

	for _, i := range data.GetItems() {
		resp.Items = append(resp.Items, &EmbyFileItem{
			Item: &model.Item{
				Name:  i.GetName(),
				Path:  fmt.Sprintf("%s/%s", aucd.ServerID, i.GetId()),
				IsDir: i.GetIsFolder(),
			},
			Type: i.GetType(),
		})
	}

	resp.Total = data.GetTotal()
	ctx.JSON(http.StatusOK, model.NewAPIDataResp(resp))
}
