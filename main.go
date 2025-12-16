package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/csv"
	"fmt"
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

type DashboardData struct {
	User         *User
	TotalSites   int64
	TotalPosts   int64
	Sites        []Site
	Posts        []Post
	FilterSiteID uint
	Sort         string
	Order        string
	ActiveTab    string
	CurrentPage  int
	TotalPages   int
	Limit        int
	HasNext      bool
	HasPrev      bool
}

// SessionManager keeps an in-memory mapping of session IDs to user IDs.
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
		"add": func(a, b int) int {
			return a + b
		},
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

	http.SetCookie(w, &http.Cookie{
		Name:    sessionCookieName,
		Value:   "",
		Path:    "/",
		Expires: time.Unix(0, 0),
	})

	http.Redirect(w, r, "/login", http.StatusFound)
}

func (a *App) handleHome(w http.ResponseWriter, r *http.Request) {
	user := r.Context().Value(userContextKey).(*User)

	activeTab := r.URL.Query().Get("tab")
	if activeTab == "" {
		activeTab = "analytics"
	}

	var totalSites, totalPosts int64
	a.db.Model(&Site{}).Count(&totalSites)
	a.db.Model(&Post{}).Count(&totalPosts)

	var sites []Site
	a.db.Order("name asc").Find(&sites)

	siteFilter := uint(0)
	if v := r.URL.Query().Get("site"); v != "" {
		if id, err := strconv.Atoi(v); err == nil {
			siteFilter = uint(id)
		}
	}

	sortField := r.URL.Query().Get("sort")
	if sortField == "" {
		sortField = "date"
	}

	orderDir := strings.ToLower(r.URL.Query().Get("order"))
	if orderDir != "asc" {
		orderDir = "desc"
	}

	// Pagination parameters
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

	orderClause := fmt.Sprintf("%s %s", mapSortColumn(sortField), orderDir)

	// Build base query for counting
	countQuery := a.db.Model(&Post{})
	if sortField == "author" {
		countQuery = countQuery.Joins("Author")
	} else if sortField == "site" {
		countQuery = countQuery.Joins("Site")
	}
	if siteFilter != 0 {
		countQuery = countQuery.Where("posts.site_id = ?", siteFilter)
	}

	// Count total matching posts
	var total int64
	countQuery.Count(&total)

	// Calculate pagination
	totalPages := int((total + int64(limit) - 1) / int64(limit))
	if totalPages == 0 {
		totalPages = 1
	}
	if page > totalPages {
		page = totalPages
	}

	offset := (page - 1) * limit

	// Build query for fetching posts
	query := a.db.Model(&Post{}).Preload("Author").Preload("Site")
	if sortField == "author" {
		query = query.Joins("Author")
	} else if sortField == "site" {
		query = query.Joins("Site")
	}
	if siteFilter != 0 {
		query = query.Where("posts.site_id = ?", siteFilter)
	}

	var posts []Post
	query.Order(orderClause).Limit(limit).Offset(offset).Find(&posts)

	data := DashboardData{
		User:         user,
		TotalSites:   totalSites,
		TotalPosts:   totalPosts,
		Sites:        sites,
		Posts:        posts,
		FilterSiteID: siteFilter,
		Sort:         sortField,
		Order:        orderDir,
		ActiveTab:    activeTab,
		CurrentPage:  page,
		TotalPages:   totalPages,
		Limit:        limit,
		HasNext:      page < totalPages,
		HasPrev:      page > 1,
	}

	a.renderTemplate(w, "home.html", data)
}

func mapSortColumn(sortField string) string {
	switch sortField {
	case "author":
		return "authors.name"
	case "site":
		return "sites.name"
	default:
		return "posts.date"
	}
}

func (a *App) handleExport(w http.ResponseWriter, r *http.Request) {
	siteFilter := uint(0)
	if v := r.URL.Query().Get("site"); v != "" {
		if id, err := strconv.Atoi(v); err == nil {
			siteFilter = uint(id)
		}
	}

	query := a.db.Model(&Post{}).Preload("Author").Preload("Site").Joins("Author").Joins("Site")
	if siteFilter != 0 {
		query = query.Where("site_id = ?", siteFilter)
	}

	var posts []Post
	query.Order("posts.date desc").Find(&posts)

	w.Header().Set("Content-Disposition", "attachment; filename=omnideck_posts.csv")
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Write([]byte{0xEF, 0xBB, 0xBF})

	writer := csv.NewWriter(w)
	defer writer.Flush()

	writer.Write([]string{"Title", "Author", "Site", "Date", "Link"})
	for _, p := range posts {
		writer.Write([]string{
			p.Title,
			p.Author.Name,
			p.Site.Name,
			p.Date.Format(time.RFC3339),
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

		site := Site{Name: name, URL: url, Status: "DOWN"}
		a.db.Create(&site)

		go CheckSite(a.db, &site)
		go CollectData(a.db, &site)

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

func (a *App) renderTemplate(w http.ResponseWriter, name string, data any) {
	if err := a.templates.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("template render error: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}
