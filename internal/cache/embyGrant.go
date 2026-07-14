package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"github.com/synctv-org/synctv/internal/db"
	"github.com/synctv-org/synctv/internal/model"
	"github.com/synctv-org/vendors/api/emby"
)

func EmbyGrantGeneration(
	movieID, creatorID, backend, serverID, rootItemID, embyUserID string,
	bindingUpdatedAt time.Time,
) (string, error) {
	if movieID == "" || creatorID == "" || serverID == "" || rootItemID == "" ||
		embyUserID == "" || bindingUpdatedAt.IsZero() {
		return "", errors.New("invalid emby grant generation context")
	}

	hash := sha256.New()
	for _, value := range []string{
		movieID,
		creatorID,
		backend,
		serverID,
		rootItemID,
		embyUserID,
		strconv.FormatInt(bindingUpdatedAt.UTC().UnixNano(), 10),
	} {
		_, _ = hash.Write([]byte(strconv.Itoa(len(value))))
		_, _ = hash.Write([]byte{':'})
		_, _ = hash.Write([]byte(value))
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func NewEmbyRootGrants(
	movieID, generation, parentItemID string,
	items []*emby.Item,
	now time.Time,
) ([]*model.EmbyRootGrant, error) {
	if movieID == "" || generation == "" || parentItemID == "" || now.IsZero() {
		return nil, errors.New("invalid emby grant context")
	}

	grants := make([]*model.EmbyRootGrant, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if item == nil || item.GetId() == "" {
			return nil, errors.New("malformed emby list item")
		}
		if _, ok := seen[item.GetId()]; ok {
			continue
		}
		seen[item.GetId()] = struct{}{}
		grants = append(grants, &model.EmbyRootGrant{
			MovieID:      movieID,
			Generation:   generation,
			ParentItemID: parentItemID,
			ChildItemID:  item.GetId(),
			IsFolder:     item.GetIsFolder(),
			GrantedAt:    now,
			ExpiresAt:    now.Add(model.EmbyRootGrantLease),
		})
	}
	return grants, nil
}

func ValidateEmbyNavigationGrant(
	movieID, generation, rootItemID, requestedItemID string,
	requireFolder bool,
	now time.Time,
) error {
	return db.AuthorizeEmbyRootGrant(
		movieID, generation, rootItemID, requestedItemID, requireFolder, now,
	)
}
