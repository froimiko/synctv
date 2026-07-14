package db

import (
	"errors"
	"time"

	"github.com/synctv-org/synctv/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	EmbyGrantMaxDepth = 64
	EmbyGrantMaxNodes = 4096
)

var (
	ErrEmbyGrantDenied   = errors.New("emby navigation authorization denied")
	ErrEmbyGrantInternal = errors.New("emby navigation grant evaluation failed")
)

type EmbyGrantError struct {
	Category string
}

func (e *EmbyGrantError) Error() string { return e.sentinel().Error() }
func (e *EmbyGrantError) Unwrap() error { return e.sentinel() }

func (e *EmbyGrantError) sentinel() error {
	if e != nil && (e.Category == "not_found" || e.Category == "not_folder") {
		return ErrEmbyGrantDenied
	}
	return ErrEmbyGrantInternal
}

func NewEmbyGrantError(category string) error {
	return &EmbyGrantError{Category: category}
}

func EmbyGrantErrorCategory(err error) string {
	var grantErr *EmbyGrantError
	if errors.As(err, &grantErr) {
		return grantErr.Category
	}
	return "unknown"
}

func UpsertEmbyRootGrants(grants []*model.EmbyRootGrant) error {
	if len(grants) == 0 {
		return nil
	}

	deduplicated := make([]*model.EmbyRootGrant, 0, len(grants))
	seen := make(map[string]struct{}, len(grants))
	for _, grant := range grants {
		if grant == nil || grant.MovieID == "" || grant.Generation == "" ||
			grant.ParentItemID == "" || grant.ChildItemID == "" || grant.ExpiresAt.IsZero() {
			continue
		}
		key := grant.MovieID + "\x00" + grant.Generation + "\x00" + grant.ParentItemID + "\x00" + grant.ChildItemID
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		deduplicated = append(deduplicated, grant)
	}
	if len(deduplicated) == 0 {
		return nil
	}

	return db.Transaction(func(tx *gorm.DB) error {
		return tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "movie_id"},
				{Name: "generation"},
				{Name: "parent_item_id"},
				{Name: "child_item_id"},
			},
			DoUpdates: clause.AssignmentColumns([]string{"is_folder", "granted_at", "expires_at"}),
		}).CreateInBatches(deduplicated, 100).Error
	})
}

func AuthorizeEmbyRootGrant(
	movieID, generation, rootItemID, targetItemID string,
	requireFolder bool,
	now time.Time,
) error {
	if movieID == "" || generation == "" || rootItemID == "" || targetItemID == "" {
		return NewEmbyGrantError("invalid_context")
	}
	if targetItemID == rootItemID {
		return nil
	}

	frontier := []string{rootItemID}
	visited := map[string]struct{}{rootItemID: {}}
	for depth := 0; depth < EmbyGrantMaxDepth && len(frontier) != 0; depth++ {
		var grants []*model.EmbyRootGrant
		err := db.Where(
			"movie_id = ? AND generation = ? AND parent_item_id IN ? AND expires_at > ?",
			movieID, generation, frontier, now,
		).Limit(EmbyGrantMaxNodes + 1).Find(&grants).Error
		if err != nil {
			return NewEmbyGrantError("database_error")
		}
		if len(grants) > EmbyGrantMaxNodes || len(visited)+len(grants) > EmbyGrantMaxNodes {
			return NewEmbyGrantError("node_limit")
		}

		next := make([]string, 0, len(grants))
		for _, grant := range grants {
			if grant == nil || grant.ChildItemID == "" {
				return NewEmbyGrantError("malformed_edge")
			}
			if grant.ChildItemID == rootItemID {
				return NewEmbyGrantError("cycle")
			}
			if _, ok := visited[grant.ChildItemID]; ok {
				continue
			}
			visited[grant.ChildItemID] = struct{}{}
			if grant.ChildItemID == targetItemID {
				if requireFolder && !grant.IsFolder {
					return NewEmbyGrantError("not_folder")
				}
				return nil
			}
			if grant.IsFolder {
				next = append(next, grant.ChildItemID)
			}
		}
		frontier = next
	}
	if len(frontier) != 0 {
		return NewEmbyGrantError("depth_limit")
	}
	return NewEmbyGrantError("not_found")
}

func deleteEmbyRootGrantsByMovieIDs(tx *gorm.DB, movieIDs []string) error {
	if len(movieIDs) == 0 {
		return nil
	}
	return tx.Where("movie_id IN ?", movieIDs).Delete(&model.EmbyRootGrant{}).Error
}

func DeleteEmbyRootGrantsByMovieID(movieID string) error {
	if movieID == "" {
		return nil
	}
	return deleteEmbyRootGrantsByMovieIDs(db, []string{movieID})
}

func DeleteEmbyRootGrantsExceptGeneration(movieID, generation string) error {
	if movieID == "" || generation == "" {
		return nil
	}
	return db.Where("movie_id = ? AND generation <> ?", movieID, generation).
		Delete(&model.EmbyRootGrant{}).Error
}

func deleteExpiredEmbyRootGrants(tx *gorm.DB, now time.Time) error {
	return tx.Where("expires_at <= ?", now).Delete(&model.EmbyRootGrant{}).Error
}

func DeleteExpiredEmbyRootGrants(now time.Time) error {
	return deleteExpiredEmbyRootGrants(db, now)
}
