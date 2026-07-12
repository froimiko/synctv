package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/synctv-org/synctv/internal/conf"
	"github.com/synctv-org/synctv/server/model"
	"github.com/synctv-org/synctv/utils"
	"github.com/synctv-org/synctv/utils/m3u8"
	"github.com/zijiren233/go-uhc"
	"github.com/zijiren233/livelib/protocol/hls"
	"github.com/zijiren233/stream"
)

const m3u8TargetTTL = 5 * time.Minute

type m3u8TargetClaims struct {
	jwt.RegisteredClaims
	RoomID     string `json:"r"`
	MovieID    string `json:"m"`
	TargetID   string `json:"t"`
	IsM3u8File bool   `json:"f"`
}

type M3u8Target struct {
	RoomID     string
	MovieID    string
	TargetURL  string
	IsM3u8File bool
}

type storedM3u8Target struct {
	roomID    string
	movieID   string
	targetURL string
	expiresAt time.Time
}

var m3u8Targets = struct {
	sync.Mutex
	items map[string]storedM3u8Target
}{items: make(map[string]storedM3u8Target)}

func storeM3u8Target(targetURL, roomID, movieID string, expiresAt time.Time) (string, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", errors.New("failed to create proxy target")
	}
	targetID := hex.EncodeToString(random)

	m3u8Targets.Lock()
	defer m3u8Targets.Unlock()
	for id, target := range m3u8Targets.items {
		if !target.expiresAt.After(time.Now()) {
			delete(m3u8Targets.items, id)
		}
	}
	m3u8Targets.items[targetID] = storedM3u8Target{
		roomID: roomID, movieID: movieID, targetURL: targetURL, expiresAt: expiresAt,
	}
	return targetID, nil
}

func loadM3u8Target(targetID, roomID, movieID string, now time.Time) (storedM3u8Target, bool) {
	m3u8Targets.Lock()
	defer m3u8Targets.Unlock()

	target, ok := m3u8Targets.items[targetID]
	if !ok || target.roomID != roomID || target.movieID != movieID || !target.expiresAt.After(now) {
		if ok && !target.expiresAt.After(now) {
			delete(m3u8Targets.items, targetID)
		}
		return storedM3u8Target{}, false
	}
	return target, true
}

func GetM3u8Target(token string) (*M3u8Target, error) {
	t, err := jwt.ParseWithClaims(token, &m3u8TargetClaims{}, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, errors.New("auth failed")
		}
		return stream.StringToBytes(conf.Conf.Jwt.Secret), nil
	})
	if err != nil || !t.Valid {
		return nil, errors.New("auth failed")
	}

	claims, ok := t.Claims.(*m3u8TargetClaims)
	if !ok || claims.TargetID == "" {
		return nil, errors.New("auth failed")
	}
	target, ok := loadM3u8Target(claims.TargetID, claims.RoomID, claims.MovieID, time.Now())
	if !ok {
		return nil, errors.New("auth failed")
	}

	return &M3u8Target{
		RoomID: claims.RoomID, MovieID: claims.MovieID, TargetURL: target.targetURL, IsM3u8File: claims.IsM3u8File,
	}, nil
}

func NewM3u8TargetToken(targetURL, roomID, movieID string, isM3u8File bool) (string, error) {
	now := time.Now()
	expiresAt := now.Add(m3u8TargetTTL)
	targetID, err := storeM3u8Target(targetURL, roomID, movieID, expiresAt)
	if err != nil {
		return "", err
	}
	claims := &m3u8TargetClaims{
		RoomID: roomID, MovieID: movieID, TargetID: targetID, IsM3u8File: isM3u8File,
		RegisteredClaims: jwt.RegisteredClaims{
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
	}

	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).
		SignedString(stream.StringToBytes(conf.Conf.Jwt.Secret))
}

const maxM3u8FileSize = 3 * 1024 * 1024 //

func M3u8Data(ctx *gin.Context, data []byte, baseURL, token, roomID, movieID string) error {
	hasM3u8File := false

	err := m3u8.RangeM3u8SegmentsWithBaseURL(
		stream.BytesToString(data),
		baseURL,
		func(segmentUrl string) (bool, error) {
			if utils.IsM3u8Url(segmentUrl) {
				hasM3u8File = true
				return false, nil
			}

			return true, nil
		},
	)
	if err != nil {
		ctx.AbortWithStatusJSON(http.StatusBadRequest,
			model.NewAPIErrorStringResp(errInvalidProxyTarget.Error()),
		)

		return errInvalidProxyTarget
	}

	m3u8Str, err := m3u8.ReplaceM3u8SegmentsWithBaseURL(
		stream.BytesToString(data),
		baseURL,
		func(segmentUrl string) (string, error) {
			targetToken, err := NewM3u8TargetToken(segmentUrl, roomID, movieID, hasM3u8File)
			if err != nil {
				return "", err
			}

			return fmt.Sprintf(
				"/api/room/movie/proxy/%s/m3u8/%s?token=%s&roomId=%s",
				movieID,
				targetToken,
				token,
				roomID,
			), nil
		},
	)
	if err != nil {
		ctx.AbortWithStatusJSON(http.StatusBadRequest,
			model.NewAPIErrorStringResp(errInvalidProxyTarget.Error()),
		)

		return errInvalidProxyTarget
	}

	ctx.Data(http.StatusOK, hls.M3U8ContentType, stream.StringToBytes(m3u8Str))

	return nil
}

// only cache non-m3u8 files
func M3u8(
	ctx *gin.Context,
	u string,
	headers map[string]string,
	isM3u8File bool,
	token, roomID, movieID string,
	opts ...Option,
) error {
	if !isM3u8File {
		return URL(ctx, u, headers, opts...)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		ctx.AbortWithStatusJSON(http.StatusBadRequest,
			model.NewAPIErrorStringResp(errInvalidProxyTarget.Error()),
		)

		return errInvalidProxyTarget
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", utils.UA)
	}

	resp, err := uhc.Do(req)
	if err != nil {
		ctx.AbortWithStatusJSON(http.StatusBadGateway,
			model.NewAPIErrorStringResp(errFailedToProxyMedia.Error()),
		)

		return errFailedToProxyMedia
	}
	defer resp.Body.Close()
	// if contentType := resp.Header.Get("Content-Type"); !strings.HasPrefix(contentType,
	// "application/vnd.apple.mpegurl") {
	// 	return fmt.Errorf("m3u8 file is not a valid m3u8 file, content type: %s", contentType)
	// }
	if resp.ContentLength > maxM3u8FileSize {
		ctx.AbortWithStatusJSON(http.StatusBadGateway,
			model.NewAPIErrorStringResp(errFailedToProxyMedia.Error()),
		)

		return errFailedToProxyMedia
	}

	b, err := io.ReadAll(io.LimitReader(resp.Body, maxM3u8FileSize))
	if err != nil {
		ctx.AbortWithStatusJSON(http.StatusBadGateway,
			model.NewAPIErrorStringResp(errFailedToProxyMedia.Error()),
		)

		return errFailedToProxyMedia
	}

	return M3u8Data(ctx, b, u, token, roomID, movieID)
}
