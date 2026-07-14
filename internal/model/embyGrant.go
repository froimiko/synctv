package model

import "time"

const EmbyRootGrantLease = 15 * time.Minute

// EmbyRootGrant records query-scope reachability, not physical Emby parentage.
type EmbyRootGrant struct {
	MovieID      string    `gorm:"primaryKey;type:char(32);index:idx_emby_grant_child,priority:1;index:idx_emby_grant_parent,priority:1"`
	Generation   string    `gorm:"primaryKey;type:char(64);index:idx_emby_grant_child,priority:2;index:idx_emby_grant_parent,priority:2"`
	ParentItemID string    `gorm:"primaryKey;type:varchar(128);index:idx_emby_grant_parent,priority:3"`
	ChildItemID  string    `gorm:"primaryKey;type:varchar(128);index:idx_emby_grant_child,priority:3"`
	IsFolder     bool      `gorm:"not null"`
	GrantedAt    time.Time `gorm:"not null"`
	ExpiresAt    time.Time `gorm:"not null;index:idx_emby_grant_child,priority:4;index:idx_emby_grant_parent,priority:4;index"`
}
