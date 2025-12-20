package main

import "time"

// User represents an application user.
type User struct {
	ID       uint   `gorm:"primaryKey"`
	Username string `gorm:"uniqueIndex;not null"`
	Password string `gorm:"not null"`
	DarkMode bool
}

// Site stores WordPress site metadata and uptime status.
type Site struct {
	ID          uint   `gorm:"primaryKey"`
	Name        string `gorm:"not null"`
	URL         string `gorm:"not null"`
	Status      string `gorm:"default:DOWN"`
	LastChecked time.Time
	Authors     []Author
	Posts       []Post
}

// Author links WordPress author information to a site.
type Author struct {
	ID     uint   `gorm:"primaryKey"`
	Name   string `gorm:"not null"`
	WPID   int    `gorm:"index"`
	SiteID uint   `gorm:"index"`
	Site   Site
	Posts  []Post
}

// Post stores WordPress post metadata.
type Post struct {
	ID           uint      `gorm:"primaryKey"`
	Title        string    `gorm:"not null"`
	Date         time.Time `gorm:"index"`
	Link         string    `gorm:"not null"`
	CommentCount int       `gorm:"default:0"`
	SiteID       uint      `gorm:"index"`
	Site         Site
	AuthorID     uint `gorm:"index"`
	Author       Author
}
