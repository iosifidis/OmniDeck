package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/csv"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

const sessionCookieName = "omnideck_session"

var userContextKey = &contextKey{name: "user"}

type contextKey struct{ name string }

type App struct {
	db        *gorm.DB
	templates *template.Template
	sessions  *SessionManager
}

// Struct matches HTML exactly now
type DashboardData struct {
	User             *User
	Sites            []Site
	Posts            []Post // CHANGED TO 'Posts' for simplicity
	TotalSites       int64
	TotalPostsDB     int64
	FilteredCount    int64
	SelectedSiteID   uint
	SelectedSiteName string
	StartDate        string
	EndDate          string
	Limit            int
	CurrentPage      int
	TotalPages       int
	HasPrev          bool
	HasNext          bool
	PrevPage         int
	NextPage         int
	ActiveTab        string
}

type SessionManager struct {
	mu    sync.RWMutex
	store map[string]uint
}

func (s *SessionManager) Create(userID uint) (string, error) {
	if s.store == nil {
		s.store = make(map[string]uint)
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	sessionID := base64.RawURLEncoding.EncodeToString(b)

	s.mu.Lock()
	s.store[sessionID] = userID
	s.mu.Unlock()
	return sessionID, nil
}

func (s *SessionManager) Get(sessionID string) (uint, bool) {
	s.mu.RLock()
	id, ok := s.store[sessionID]
	s.mu.RUnlock()
	return id, ok
}

func (s *SessionManager) Delete(sessionID string) {
	s.mu.Lock()
	delete(s.store, sessionID)
	s.mu.Unlock()
}

func main() {
	db, err := gorm.Open(sqlite.Open("data.db"), &gorm.Config{})
	if err != nil {
		log.Fatalf("failed to connect database: %v", err)
	}

	if err := db.AutoMigrate(&User{}, &Site{}, &Author{}, &Post{}); err != nil {
		log.Fatalf("failed to migrate database: %v", err)
	}

	if err := ensureDefaultAdmin(db); err != nil {
		log.Fatalf("failed to ensure default admin: %v", err)
	}

	funcMap := template.FuncMap{
		"add": func(a, b int) int { return a + b },
	}

	tmpl := template.Must(template.New("").Funcs(funcMap).ParseGlob("templates/*.html"))

	app := &App{
		db:        db,
		templates: tmpl,
		sessions:  &SessionManager{store: make(map[string]uint)},
	}

	stopMonitor := make(chan struct{})
	StartMonitoring(db, 5*time.Minute, stopMonitor)

	mux := http.NewServeMux()
	mux.HandleFunc("/login", app.handleLogin)
	mux.HandleFunc("/logout", app.handleLogout)
	mux.HandleFunc("/", app.handleHome)
	mux.HandleFunc("/export", app.handleExport)
	mux.HandleFunc("/settings", app.handleSettings)
	mux.HandleFunc("/site/edit", app.handleEditSite)
	mux.HandleFunc("/site/delete", app.handleDeleteSite)
	mux.HandleFunc("/profile", app.handleProfile)
	mux.HandleFunc("/reset-data", app.handleResetData)
	mux.HandleFunc("/sync-all", app.handleSyncAll)
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))

	server := &http.Server{
		Addr:    ":8080",
		Handler: app.authMiddleware(mux),
	}

	log.Printf("OmniDeck running on http://localhost%v", server.Addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server failed: %v", err)
	}
	close(stopMonitor)
}

func ensureDefaultAdmin(db *gorm.DB) error {
	var count int64
	db.Model(&User{}).Count(&count)
	if count > 0 {
		return nil
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("admin"), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	user := User{Username: "admin", Password: string(hash), DarkMode: false}
	return db.Create(&user).Error
}

func (a *App) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" || strings.HasPrefix(r.URL.Path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		user := a.currentUser(r)
		if user == nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		ctx := context.WithValue(r.Context(), userContextKey, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *App) currentUser(r *http.Request) *User {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return nil
	}
	userID, ok := a.sessions.Get(cookie.Value)
	if !ok {
		return nil
	}
	var user User
	if err := a.db.First(&user, userID).Error; err != nil {
		return nil
	}
	return &user
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		a.renderTemplate(w, "login.html", nil)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	var user User
	if err := a.db.Where("username = ?", username).First(&user).Error; err != nil {
		a.renderTemplate(w, "login.html", map[string]string{"Error": "Invalid credentials"})
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(password)); err != nil {
		a.renderTemplate(w, "login.html", map[string]string{"Error": "Invalid credentials"})
		return
	}
	sessionID, err := a.sessions.Create(user.ID)
	if err != nil {
		log.Printf("failed to create session: %v", err)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		Expires:  time.Now().Add(24 * time.Hour),
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(sessionCookieName)
	if err == nil {
		a.sessions.Delete(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "", Path: "/", Expires: time.Unix(0, 0)})
	http.Redirect(w, r, "/login", http.StatusFound)
}

func (a *App) handleHome(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userContextKey).(*User)
	activeTab := r.URL.Query().Get("tab")
	if activeTab == "" {
		activeTab = "analytics"
	}

	var totalSites, totalPostsDB int64
	a.db.Model(&Site{}).Count(&totalSites)
	a.db.Model(&Post{}).Count(&totalPostsDB)

	var sites []Site
	a.db.Order("name asc").Find(&sites)

	siteFilter := uint(0)
	selectedSiteName := "All Sites"
	if v := r.URL.Query().Get("site_id"); v != "" {
		if id, err := strconv.Atoi(v); err == nil {
			siteFilter = uint(id)
			for _, site := range sites {
				if site.ID == siteFilter {
					selectedSiteName = site.Name
					break
				}
			}
		}
	}

	page := 1
	if v := r.URL.Query().Get("page"); v != "" {
		if p, err := strconv.Atoi(v); err == nil && p > 0 {
			page = p
		}
	}
	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		if l, err := strconv.Atoi(v); err == nil && l > 0 && l <= 100 {
			limit = l
		}
	}

	startDateStr := r.URL.Query().Get("start_date")
	endDateStr := r.URL.Query().Get("end_date")
	now := time.Now()
	endDate := startOfDay(now).Add(24*time.Hour - time.Nanosecond)
	startDate := startOfDay(now.AddDate(0, -1, 0))

	if endDateStr != "" {
		if parsed, err := time.Parse("2006-01-02", endDateStr); err == nil {
			endDate = startOfDay(parsed).Add(24*time.Hour - time.Nanosecond)
		}
	}
	if startDateStr != "" {
		if parsed, err := time.Parse("2006-01-02", startDateStr); err == nil {
			startDate = startOfDay(parsed)
		}
	}
	if startDateStr == "" {
		startDateStr = startDate.Format("2006-01-02")
	}
	if endDateStr == "" {
		endDateStr = endDate.Format("2006-01-02")
	}

	countQuery := a.db.Model(&Post{})
	if siteFilter != 0 {
		countQuery = countQuery.Where("site_id = ?", siteFilter)
	}
	countQuery = countQuery.Where("date >= ? AND date <= ?", startDate, endDate)
	var filteredTotal int64
	countQuery.Count(&filteredTotal)

	// Historical data fetching: if no results and date range is set, fetch from API
	if filteredTotal == 0 && (startDateStr != "" || endDateStr != "") {
		var sitesToFetch []Site
		if siteFilter != 0 {
			a.db.First(&sitesToFetch, siteFilter)
			sitesToFetch = []Site{sitesToFetch[0]}
		} else {
			a.db.Find(&sitesToFetch)
		}

		for _, s := range sitesToFetch {
			siteCopy := s
			if err := FetchArchive(a.db, &siteCopy, startDate, endDate); err != nil {
				log.Printf("FetchArchive failed for site %d: %v", siteCopy.ID, err)
			}
		}

		// Re-query after fetching
		countQuery = a.db.Model(&Post{})
		if siteFilter != 0 {
			countQuery = countQuery.Where("site_id = ?", siteFilter)
		}
		countQuery = countQuery.Where("date >= ? AND date <= ?", startDate, endDate)
		countQuery.Count(&filteredTotal)
	}

	totalPages := int((filteredTotal + int64(limit) - 1) / int64(limit))
	if totalPages == 0 {
		totalPages = 1
	}
	if page > totalPages {
		page = totalPages
	}
	offset := (page - 1) * limit

	query := a.db.Model(&Post{}).Preload("Author").Preload("Site")
	if siteFilter != 0 {
		query = query.Where("posts.site_id = ?", siteFilter)
	}
	query = query.Where("posts.date >= ? AND posts.date <= ?", startDate, endDate)

	var posts []Post
	query.Order("date desc").Limit(limit).Offset(offset).Find(&posts)

	data := DashboardData{
		User:             user,
		TotalSites:       totalSites,
		TotalPostsDB:     totalPostsDB,
		FilteredCount:    filteredTotal,
		SelectedSiteName: selectedSiteName,
		Sites:            sites,
		Posts:            posts, // Correctly assigned
		SelectedSiteID:   siteFilter,
		ActiveTab:        activeTab,
		CurrentPage:      page,
		TotalPages:       totalPages,
		Limit:            limit,
		HasNext:          page < totalPages,
		HasPrev:          page > 1,
		NextPage:         page + 1,
		PrevPage:         page - 1,
		StartDate:        startDateStr,
		EndDate:          endDateStr,
	}

	a.renderTemplate(w, "home.html", data)
}

func (a *App) handleExport(w http.ResponseWriter, r *http.Request) {
	siteFilter := uint(0)
	if v := r.URL.Query().Get("site_id"); v != "" {
		if id, err := strconv.Atoi(v); err == nil {
			siteFilter = uint(id)
		}
	}
	query := a.db.Model(&Post{}).Preload("Author").Preload("Site")
	if siteFilter != 0 {
		query = query.Where("site_id = ?", siteFilter)
	}
	startDateStr := r.URL.Query().Get("start_date")
	endDateStr := r.URL.Query().Get("end_date")
	if startDateStr != "" && endDateStr != "" {
		query = query.Where("date >= ? AND date <= ?", startDateStr+" 00:00:00", endDateStr+" 23:59:59")
	}
	var posts []Post
	query.Order("date desc").Find(&posts)
	w.Header().Set("Content-Disposition", "attachment; filename=omnideck_posts.csv")
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Write([]byte{0xEF, 0xBB, 0xBF})
	writer := csv.NewWriter(w)
	defer writer.Flush()
	writer.Write([]string{"Date", "Title", "Author", "Site", "Link"})
	for _, p := range posts {
		authorName := "Unknown"
		if p.AuthorID != 0 {
			var auth Author
			if a.db.First(&auth, p.AuthorID).Error == nil {
				authorName = auth.Name
			}
		}
		writer.Write([]string{
			p.Date.Format("2006-01-02"),
			p.Title,
			authorName,
			p.Site.Name,
			p.Link,
		})
	}
}

func (a *App) handleSettings(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userContextKey).(*User)
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		name := strings.TrimSpace(r.FormValue("name"))
		url := strings.TrimSpace(r.FormValue("url"))
		if name == "" || url == "" {
			http.Redirect(w, r, "/settings", http.StatusSeeOther)
			return
		}
		site := Site{Name: name, URL: url, Status: "DOWN", LastChecked: time.Now()}
		a.db.Create(&site)
		go func() {
			CheckSite(a.db, &site)
			CollectData(a.db, &site)
		}()
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	var sites []Site
	a.db.Order("name asc").Find(&sites)
	data := struct {
		User  *User
		Sites []Site
	}{User: user, Sites: sites}
	a.renderTemplate(w, "settings.html", data)
}

func (a *App) handleEditSite(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userContextKey).(*User)
	idStr := r.URL.Query().Get("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	var site Site
	if err := a.db.First(&site, id).Error; err != nil {
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		site.Name = strings.TrimSpace(r.FormValue("name"))
		site.URL = strings.TrimSpace(r.FormValue("url"))
		a.db.Save(&site)
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	data := struct {
		User *User
		Site Site
	}{User: user, Site: site}
	a.renderTemplate(w, "site_edit.html", data)
}

func (a *App) handleDeleteSite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	idStr := r.URL.Query().Get("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	a.db.Where("site_id = ?", id).Delete(&Post{})
	a.db.Where("site_id = ?", id).Delete(&Author{})
	a.db.Delete(&Site{}, id)
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (a *App) handleProfile(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userContextKey).(*User)
	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		username := strings.TrimSpace(r.FormValue("username"))
		password := r.FormValue("password")
		darkMode := r.FormValue("dark_mode") == "on"
		if username != "" {
			user.Username = username
		}
		if password != "" {
			hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
			if err != nil {
				http.Error(w, "could not hash password", http.StatusInternalServerError)
				return
			}
			user.Password = string(hash)
		}
		user.DarkMode = darkMode
		if err := a.db.Save(user).Error; err != nil {
			http.Error(w, "failed to update profile", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/profile", http.StatusSeeOther)
		return
	}
	data := struct {
		User *User
	}{User: user}
	a.renderTemplate(w, "profile.html", data)
}

func (a *App) handleResetData(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Delete all posts and authors
	if err := a.db.Exec("DELETE FROM posts").Error; err != nil {
		log.Printf("Error deleting posts: %v", err)
		http.Error(w, "Failed to clear posts", http.StatusInternalServerError)
		return
	}

	if err := a.db.Exec("DELETE FROM authors").Error; err != nil {
		log.Printf("Error deleting authors: %v", err)
		http.Error(w, "Failed to clear authors", http.StatusInternalServerError)
		return
	}

	// Reclaim disk space in SQLite
	if err := a.db.Exec("VACUUM").Error; err != nil {
		log.Printf("Warning: VACUUM failed: %v", err)
	}

	log.Println("Database cleared: all posts and authors deleted")
	http.Redirect(w, r, "/profile", http.StatusSeeOther)
}

func (a *App) handleSyncAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Fetch all sites from database
	var sites []Site
	if err := a.db.Find(&sites).Error; err != nil {
		log.Printf("Error fetching sites: %v", err)
		http.Error(w, "Failed to fetch sites", http.StatusInternalServerError)
		return
	}

	// Trigger data collection for each site in background
	for i := range sites {
		site := &sites[i]
		go CollectData(a.db, site)
		log.Printf("Triggered sync for site: %s", site.Name)
	}

	log.Printf("Manual sync triggered for %d sites", len(sites))
	http.Redirect(w, r, "/?tab=monitoring", http.StatusSeeOther)
}

func (a *App) renderTemplate(w http.ResponseWriter, name string, data any) {
	if err := a.templates.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("template render error: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}
