package main

import (
	"log"
	"net/http"
	"time"

	"gorm.io/gorm"
)

// CheckSite performs a basic HTTP GET to determine uptime for a site.
func CheckSite(db *gorm.DB, site *Site) {
	client := &http.Client{Timeout: 10 * time.Second}
	status := "DOWN"

	resp, err := client.Get(site.URL)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode < 400 {
			status = "UP"
		}
	} else {
		log.Printf("site check failed for %s: %v", site.URL, err)
	}

	site.Status = status
	site.LastChecked = time.Now()
	db.Save(site)
}

// StartMonitoring periodically checks all sites on an interval.
func StartMonitoring(db *gorm.DB, interval time.Duration, stop <-chan struct{}) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				var sites []Site
				db.Find(&sites)
				for _, s := range sites {
					siteCopy := s
					go func(site *Site) {
						defer func() {
							if r := recover(); r != nil {
								log.Printf("Recovered from panic in site monitor for %s: %v", site.Name, r)
							}
						}()
						CheckSite(db, site)
						if err := CollectData(db, site); err != nil {
							log.Printf("error collecting data for site %s: %v", site.Name, err)
						}
					}(&siteCopy)
				}
			case <-stop:
				return
			}
		}
	}()
}
