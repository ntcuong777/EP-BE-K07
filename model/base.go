package model

import (
	"gorm.io/gorm"
	"time"
)

type Base struct {
	gorm.Model

	ID        uint      `gorm:"id;primaryKey"`
	CreatedAt time.Time `gorm:"created_at;not null" json:"createdAt"`
	UpdatedAt time.Time `gorm:"updated_at;not null" json:"updatedAt"`
}
