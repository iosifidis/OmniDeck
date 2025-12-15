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
	endpoint := fmt.Sprintf("%s/wp-json/wp/v2/posts?_embed", strings.TrimSuffix(site.URL, "/"))

	resp, err := client.Get(endpoint)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("unexpected status %d from %s", resp.StatusCode, endpoint)
	}

	var wpPosts []wpPost
	if err := json.NewDecoder(resp.Body).Decode(&wpPosts); err != nil {
		return err
	}

	for _, p := range wpPosts {
		publishedAt, err := time.Parse(time.RFC3339, p.Date)
		if err != nil {
			log.Printf("unable to parse post date %q: %v", p.Date, err)
			publishedAt = time.Now()
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
		post.SiteID = site.ID
		post.AuthorID = author.ID
		if post.ID == 0 {
			db.Create(&post)
		} else {
			db.Save(&post)
		}
	}

	return nil
}

type wpPost struct {
	ID    int    `json:"id"`
	Date  string `json:"date"`
	Title struct {
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
