package vendoremby

import (
	"context"
	"errors"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/synctv-org/synctv/internal/cache"
	"github.com/synctv-org/synctv/internal/db"
	dbModel "github.com/synctv-org/synctv/internal/model"
	"github.com/synctv-org/synctv/internal/op"
	"github.com/synctv-org/synctv/internal/vendor"
	"github.com/synctv-org/vendors/api/emby"
)

// ErrEmbyCredentialsUnavailable reports that a binding cannot be refreshed
// automatically because no usable credentials are stored for it.
//
// This is the expected outcome for bindings created before credential storage
// existed, and for deployments that leave the vendor credential secret blank.
// It is deliberately distinct from a refresh that was attempted and failed, so
// callers can tell "cannot try" apart from "tried and lost".
var ErrEmbyCredentialsUnavailable = errors.New("emby credentials unavailable")

// ErrEmbyServerIDMismatch reports that a re-login landed on a different Emby
// server than the binding being refreshed.
var ErrEmbyServerIDMismatch = errors.New("emby server id mismatch")

// isEmbyUnauthorized reports whether err is an Emby authentication failure.
//
// This has to match on error text, which is ugly but forced by the module
// boundary: the vendors module flattens every upstream failure into a plain
// error string, either fmt.Errorf("status code %d: %s", ...) (see
// vendors/vendors/emby/user.go and library.go) or errors.New(string(body)) in
// GetAPIKey. No typed error survives the gRPC hop, so there is nothing to
// errors.As against.
//
// Revisit this the moment the vendors module starts returning typed errors or
// gRPC status codes.
//
// It is deliberately conservative: only a 401 status or an explicit
// "invalid token" body counts. Other 4xx responses (403 permission denied, 404
// missing item) are NOT treated as unauthorized, because re-authenticating
// would not fix them and would burn a login round-trip on every such error.
func isEmbyUnauthorized(err error) bool {
	if err == nil {
		return false
	}

	msg := strings.ToLower(err.Error())

	return strings.Contains(msg, "status code 401") || strings.Contains(msg, "invalid token")
}

// embyTokenRefreshFunc re-authenticates a binding and returns refreshed cache
// data. It is injected into withEmbyTokenRefresh so the retry policy can be
// exercised without a database.
type embyTokenRefreshFunc func(ctx context.Context) (*cache.EmbyUserCacheData, error)

// refreshEmbyToken re-authenticates the user against Emby using the stored
// credentials, persists the new session token and refreshes the user cache.
//
// Emby hands out session tokens from /emby/Users/authenticatebyname, not
// permanent API keys, and reclaims them once a session goes idle. Without this
// the user has to unbind and re-enter their credentials by hand.
//
// Nothing here logs the password, the token, or any URL that could carry them.
func refreshEmbyToken(
	ctx context.Context,
	user *op.User,
	serverID string,
) (*cache.EmbyUserCacheData, error) {
	if user == nil || serverID == "" {
		return nil, ErrEmbyCredentialsUnavailable
	}

	v, err := db.GetEmbyVendor(user.ID, serverID)
	if err != nil {
		return nil, err
	}

	// Empty credentials mean either a pre-existing binding or a deployment with
	// no credential secret configured (the model hooks blank the password out
	// rather than encrypt it under a row-derivable key).
	if v.Username == "" || len(v.Password) == 0 {
		return nil, ErrEmbyCredentialsUnavailable
	}

	data, err := vendor.LoadEmbyClient(v.Backend).Login(ctx, &emby.LoginReq{
		Host:     v.Host,
		Username: v.Username,
		Password: string(v.Password),
	})
	if err != nil {
		deleteEmbyCachedBinding(user.EmbyCache(), serverID)
		return nil, err
	}

	if err := validateEmbyLoginResponse(data); err != nil {
		deleteEmbyCachedBinding(user.EmbyCache(), serverID)
		return nil, err
	}

	// Never let a login against a different server overwrite this binding.
	if data.GetServerId() != serverID {
		deleteEmbyCachedBinding(user.EmbyCache(), serverID)
		return nil, ErrEmbyServerIDMismatch
	}

	// CreateOrSaveEmbyVendor does a full Save, overwriting every column, so the
	// credentials must be re-supplied here or the refresh would wipe the very
	// data it depends on for the next refresh.
	persisted, err := db.CreateOrSaveEmbyVendor(&dbModel.EmbyVendor{
		UserID:     user.ID,
		ServerID:   serverID,
		Host:       v.Host,
		APIKey:     data.GetToken(),
		Backend:    v.Backend,
		EmbyUserID: data.GetUserId(),
		Username:   v.Username,
		Password:   v.Password,
	})
	if err != nil {
		deleteEmbyCachedBinding(user.EmbyCache(), serverID)
		return nil, err
	}

	cacheData, err := embyUserCacheDataFromVendor(persisted)
	if err != nil {
		deleteEmbyCachedBinding(user.EmbyCache(), serverID)
		return nil, err
	}

	if _, err := user.EmbyCache().StoreOrRefreshWithDynamicFunc(
		ctx,
		serverID,
		func(context.Context, string) (*cache.EmbyUserCacheData, error) {
			return cacheData, nil
		},
	); err != nil {
		deleteEmbyCachedBinding(user.EmbyCache(), serverID)
		return nil, err
	}

	return cacheData, nil
}

// withEmbyTokenRefresh runs call, and on an unauthorized response refreshes the
// session token exactly once before retrying.
//
// It never loops: the retry is attempted at most one time, so a server that
// rejects even a freshly minted token cannot spin. If the refresh itself fails,
// the ORIGINAL unauthorized error is returned rather than the refresh error,
// because the former describes what the caller actually asked for; the refresh
// failure is logged instead.
func withEmbyTokenRefresh[T any](
	ctx context.Context,
	refresh embyTokenRefreshFunc,
	call func(ctx context.Context, aucd *cache.EmbyUserCacheData) (T, error),
	aucd *cache.EmbyUserCacheData,
) (T, error) {
	result, err := call(ctx, aucd)
	if !isEmbyUnauthorized(err) {
		return result, err
	}

	unauthorizedErr := err

	if refresh == nil {
		return result, unauthorizedErr
	}

	refreshed, refreshErr := refresh(ctx)
	if refreshErr != nil {
		// Log without credentials: no password, no token, no host URL.
		log.WithField("error", refreshErr.Error()).
			Warn("emby token refresh failed, returning original unauthorized error")

		return result, unauthorizedErr
	}

	if refreshed == nil {
		return result, unauthorizedErr
	}

	return call(ctx, refreshed)
}
