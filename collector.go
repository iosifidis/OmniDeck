package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"gorm.io/gorm"
)

// CollectData fetches recent posts from the WordPress REST API and persists them.
func CollectData(db *gorm.DB, site *Site) error {
	client := &http.Client{Timeout: 15 * time.Second}
	baseURL := strings.TrimSuffix(site.URL, "/")

	// Fetch all pages of posts
	page := 1
	for {
		endpoint := fmt.Sprintf("%s/wp-json/wp/v2/posts?_embed&per_page=100&page=%d", baseURL, page)

		resp, err := client.Get(endpoint)
		if err != nil {
			if page == 1 {
				return err
			}
			break // Stop if we can't fetch more pages
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 400 {
			if page == 1 {
				return fmt.Errorf("unexpected status %d from %s", resp.StatusCode, endpoint)
			}
			break // No more pages
		}

		var wpPosts []wpPost
		if err := json.NewDecoder(resp.Body).Decode(&wpPosts); err != nil {
			if page == 1 {
				return err
			}
			break
		}

		if len(wpPosts) == 0 {
			break // No more posts
		}

		// Process posts
		for _, p := range wpPosts {
			saveSinglePost(db, site, p)
		}

		// Check if there are more pages
		totalPages := resp.Header.Get("X-WP-TotalPages")
		if totalPages == "" || page >= 10 { // Safety limit: max 10 pages (1000 posts)
			break
		}

		page++
	}

	return nil
}

// saveSinglePost saves or updates a single post from WordPress
func saveSinglePost(db *gorm.DB, site *Site, p wpPost) {
	// Try to parse the WordPress date string
	layout := "2006-01-02T15:04:05"
	publishedAt, err := time.Parse(time.RFC3339, p.Date)
	if err != nil {
		// Fallback to simpler format without timezone
		publishedAt, err = time.Parse(layout, p.Date)
		if err != nil {
			log.Printf("unable to parse post date %q: %v", p.Date, err)
			publishedAt = time.Now()
		}
	}

	authorName := "Unknown"
	wpAuthorID := 0
	if len(p.Embedded.Author) > 0 {
		authorName = p.Embedded.Author[0].Name
		wpAuthorID = p.Embedded.Author[0].ID
	}

	var author Author
	db.Where("wpid = ? AND site_id = ?", wpAuthorID, site.ID).First(&author)
	if author.ID == 0 {
		author = Author{Name: authorName, WPID: wpAuthorID, SiteID: site.ID}
		db.Create(&author)
	} else if author.Name != authorName {
		author.Name = authorName
		db.Save(&author)
	}

	var post Post
	db.Where("link = ? AND site_id = ?", p.Link, site.ID).First(&post)
	post.Title = p.Title.Rendered
	post.Date = publishedAt
	post.Link = p.Link
	post.CommentCount = p.CommentCount
	post.SiteID = site.ID
	post.AuthorID = author.ID
	if post.ID == 0 {
		db.Create(&post)
	} else {
		db.Save(&post)
	}
}

// FetchArchive fetches historical posts from the WordPress REST API within a date range.
func FetchArchive(db *gorm.DB, site *Site, startDate, endDate time.Time) error {
	client := &http.Client{Timeout: 15 * time.Second}
	baseURL := strings.TrimSuffix(site.URL, "/")

	// Format dates in ISO8601 format (before and after params are inclusive/exclusive)
	afterParam := startDate.Format("2006-01-02T15:04:05")
	beforeParam := endDate.Format("2006-01-02T15:04:05")

	// Fetch all pages of posts within the date range
	page := 1
	for {
		endpoint := fmt.Sprintf(
			"%s/wp-json/wp/v2/posts?_embed&after=%s&before=%s&per_page=100&page=%d",
			baseURL,
			afterParam,
			beforeParam,
			page,
		)

		resp, err := client.Get(endpoint)
		if err != nil {
			if page == 1 {
				return err
			}
			break
		}
		defer resp.Body.Close()

		if resp.StatusCode >= 400 {
			if page == 1 {
				return fmt.Errorf("unexpected status %d from %s", resp.StatusCode, endpoint)
			}
			break
		}

		var wpPosts []wpPost
		if err := json.NewDecoder(resp.Body).Decode(&wpPosts); err != nil {
			if page == 1 {
				return err
			}
			break
		}

		if len(wpPosts) == 0 {
			break
		}

		// Save posts using the same logic as CollectData
		for _, p := range wpPosts {
			saveSinglePost(db, site, p)
		}

		// Check if there are more pages
		totalPages := resp.Header.Get("X-WP-TotalPages")
		if totalPages == "" || page >= 10 { // Safety limit: max 10 pages (1000 posts)
			break
		}

		page++
	}

	return nil
}

type wpPost struct {
	ID           int    `json:"id"`
	Date         string `json:"date"`
	CommentCount int    `json:"comment_count"`
	Title        struct {
		Rendered string `json:"rendered"`
	} `json:"title"`
	Link     string `json:"link"`
	Embedded struct {
		Author []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"author"`
	} `json:"_embedded"`
}
