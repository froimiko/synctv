package db

import (
	"github.com/synctv-org/synctv/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	ErrRoomOrMovieNotFound = "room or movie"
)

func CreateMovie(movie *model.Movie) error {
	return db.Create(movie).Error
}

func CreateMovies(movies []*model.Movie) error {
	return db.CreateInBatches(movies, 100).Error
}

func WithParentMovieID(parentMovieID string) func(*gorm.DB) *gorm.DB {
	return func(db *gorm.DB) *gorm.DB {
		if parentMovieID == "" {
			return db.Where("base_parent_id IS NULL")
		}
		return db.Where("base_parent_id = ?", parentMovieID)
	}
}

func GetMoviesByRoomID(roomID string, scopes ...func(*gorm.DB) *gorm.DB) ([]*model.Movie, error) {
	var movies []*model.Movie

	err := db.Where("room_id = ?", roomID).
		Order("position ASC").
		Scopes(scopes...).
		Find(&movies).
		Error

	return movies, err
}

func GetMoviesCountByRoomID(roomID string, scopes ...func(*gorm.DB) *gorm.DB) (int64, error) {
	var count int64

	err := db.Model(&model.Movie{}).
		Where("room_id = ?", roomID).
		Scopes(scopes...).
		Count(&count).
		Error

	return count, err
}

func GetMovieByID(roomID, id string, scopes ...func(*gorm.DB) *gorm.DB) (*model.Movie, error) {
	var movie model.Movie

	err := db.Where("room_id = ? AND id = ?", roomID, id).Scopes(scopes...).First(&movie).Error
	return &movie, HandleNotFound(err, ErrRoomOrMovieNotFound)
}

func movieDescendantIDs(movies []*model.Movie, rootIDs []string) []string {
	children := make(map[string][]string, len(movies))
	for _, movie := range movies {
		if movie == nil || movie.ID == "" {
			continue
		}
		children[movie.ParentID.String()] = append(children[movie.ParentID.String()], movie.ID)
	}

	ids := make([]string, 0, len(rootIDs))
	visited := make(map[string]struct{}, len(rootIDs))
	queue := append([]string(nil), rootIDs...)
	for len(queue) != 0 {
		id := queue[0]
		queue = queue[1:]
		if id == "" {
			continue
		}
		if _, ok := visited[id]; ok {
			continue
		}
		visited[id] = struct{}{}
		ids = append(ids, id)
		queue = append(queue, children[id]...)
	}
	return ids
}

func deleteMoviesWithDescendants(
	tx *gorm.DB,
	roomID string,
	rootScopes []func(*gorm.DB) *gorm.DB,
	unscoped bool,
) error {
	rootQuery := tx.Model(&model.Movie{}).Where("room_id = ?", roomID).Scopes(rootScopes...)
	if unscoped {
		rootQuery = rootQuery.Unscoped()
	}
	var rootIDs []string
	if err := rootQuery.Pluck("id", &rootIDs).Error; err != nil {
		return err
	}
	if len(rootIDs) == 0 {
		return NotFoundError(ErrRoomOrMovieNotFound)
	}

	allQuery := tx.Where("room_id = ?", roomID)
	if unscoped {
		allQuery = allQuery.Unscoped()
	}
	var movies []*model.Movie
	if err := allQuery.Find(&movies).Error; err != nil {
		return err
	}
	ids := movieDescendantIDs(movies, rootIDs)
	if err := deleteEmbyRootGrantsByMovieIDs(tx, ids); err != nil {
		return err
	}

	deleteQuery := tx.Where("room_id = ? AND id IN ?", roomID, ids)
	if unscoped {
		deleteQuery = deleteQuery.Unscoped()
	}
	return HandleUpdateResult(deleteQuery.Delete(&model.Movie{}), ErrRoomOrMovieNotFound)
}

func DeleteMovieByID(roomID, id string) error {
	return db.Transaction(func(tx *gorm.DB) error {
		return deleteMoviesWithDescendants(tx, roomID, []func(*gorm.DB) *gorm.DB{
			func(tx *gorm.DB) *gorm.DB { return tx.Where("id = ?", id) },
		}, true)
	})
}

func DeleteMoviesByID(roomID string, ids []string) error {
	return db.Transaction(func(tx *gorm.DB) error {
		return deleteMoviesWithDescendants(tx, roomID, []func(*gorm.DB) *gorm.DB{
			func(tx *gorm.DB) *gorm.DB { return tx.Where("id IN ?", ids) },
		}, true)
	})
}

func DeleteMoviesByRoomID(roomID string, scopes ...func(*gorm.DB) *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		return deleteMoviesWithDescendants(tx, roomID, scopes, false)
	})
}

func DeleteMoviesByRoomIDAndParentID(roomID, parentID string) error {
	return DeleteMoviesByRoomID(roomID, WithParentMovieID(parentID))
}

func UpdateMovie(movie *model.Movie, columns ...clause.Column) error {
	return db.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(movie).
			Clauses(clause.Returning{Columns: columns}).
			Where("room_id = ? AND id = ?", movie.RoomID, movie.ID).
			Updates(movie)
		if err := HandleUpdateResult(result, ErrRoomOrMovieNotFound); err != nil {
			return err
		}
		return deleteEmbyRootGrantsByMovieIDs(tx, []string{movie.ID})
	})
}

func SaveMovie(movie *model.Movie, columns ...clause.Column) error {
	return db.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(movie).
			Clauses(clause.Returning{Columns: columns}).
			Where("room_id = ? AND id = ?", movie.RoomID, movie.ID).
			Omit("created_at").
			Save(movie)
		if err := HandleUpdateResult(result, ErrRoomOrMovieNotFound); err != nil {
			return err
		}
		return deleteEmbyRootGrantsByMovieIDs(tx, []string{movie.ID})
	})
}

func SwapMoviePositions(roomID, movie1ID, movie2ID string) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var movie1, movie2 model.Movie
		if err := tx.Where("room_id = ? AND id = ?", roomID, movie1ID).First(&movie1).Error; err != nil {
			return HandleNotFound(err, ErrRoomOrMovieNotFound)
		}

		if err := tx.Where("room_id = ? AND id = ?", roomID, movie2ID).First(&movie2).Error; err != nil {
			return HandleNotFound(err, ErrRoomOrMovieNotFound)
		}

		movie1.Position, movie2.Position = movie2.Position, movie1.Position

		result1 := tx.Model(&movie1).
			Where("room_id = ? AND id = ?", roomID, movie1ID).
			Update("position", movie1.Position)
		if err := HandleUpdateResult(result1, ErrRoomOrMovieNotFound); err != nil {
			return err
		}

		result2 := tx.Model(&movie2).
			Where("room_id = ? AND id = ?", roomID, movie2ID).
			Update("position", movie2.Position)

		return HandleUpdateResult(result2, ErrRoomOrMovieNotFound)
	})
}
