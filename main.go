package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"
	intentai "github.com/mr-destructive/crossposter-agent/ai"
	"github.com/mr-destructive/crossposter-agent/auth"
	crossposter_db "github.com/mr-destructive/crossposter-agent/db"
	"github.com/mr-destructive/crossposter-agent/platforms"
	"github.com/mr-destructive/crossposter-agent/workers"
	"golang.org/x/crypto/bcrypt"
)

const sessionCookieName = "crossposter_session"

type app struct {
	tmpl    *template.Template
	db      *sql.DB
	queries *crossposter_db.Queries
	aesKey  string
	jobs    chan crossposter_db.PostJob
	rootCtx context.Context
}

var supportedPlatforms = []string{"devto", "hashnode", "bluesky", "medium", "reddit", "x", "substack"}

type DashboardJob struct {
	PostID      int64
	Title       string
	Platform    string
	Status      string
	CreatedAt   string
	ScheduledAt string
	LastError   string
	RemoteID    string
	RemoteURL   string
}

type JobEventView struct {
	Platform  string
	Stage     string
	Message   string
	CreatedAt string
}

type ChatMessageView struct {
	Role      string
	Content   string
	CreatedAt string
}

type PlatformOptionInput struct {
	Tags         []string `json:"tags,omitempty"`
	Series       string   `json:"series,omitempty"`
	PublishMode  string   `json:"publish_mode,omitempty"`
	CanonicalURL string   `json:"canonical_url,omitempty"`
	Subreddit    string   `json:"subreddit,omitempty"`
	Tone         string   `json:"tone,omitempty"`
	Audience     string   `json:"audience,omitempty"`
	CallToAction string   `json:"call_to_action,omitempty"`
	ThreadMode   string   `json:"thread_mode,omitempty"`
	Additional   string   `json:"additional,omitempty"`
}

type AIConfirmation struct {
	Platform string
	Note     string
}

type ConfiguredPlatform struct {
	Platform string
	AuthType string
}

type EditablePost struct {
	ID      int64
	Title   string
	Content string
}

type RemoteRef struct {
	RemoteID  string
	RemoteURL string
}

type DashboardStats struct {
	Pending    int
	Processing int
	Success    int
	Failed     int
}

type addPostPageData struct {
	Error         string
	Success       string
	Intent        string
	Title         string
	Content       string
	AutoSelect    bool
	ScheduledAt   string
	AIConfirm     bool
	Confirmed     bool
	PreviewMode   bool
	Confirmations []AIConfirmation
	TracePostID   int64
	TraceJobs     []DashboardJob
	TraceEvents   []JobEventView
	ThreadID      int64
	ChatInput     string
	ChatHistory   []ChatMessageView
}

type PlatformCredentialPayload struct {
	Kind                string `json:"kind"`
	Token               string `json:"token,omitempty"`
	HashnodeHost        string `json:"hashnode_host,omitempty"`
	MediumPublicationID string `json:"medium_publication_id,omitempty"`
	RedditSubreddit     string `json:"reddit_subreddit,omitempty"`
	Identifier          string `json:"identifier,omitempty"`
	AppPassword         string `json:"app_password,omitempty"`
	SubstackEndpoint    string `json:"substack_endpoint,omitempty"`
}

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Println(".env file not found, using system environment")
	}

	aesKey := os.Getenv("AES_KEY")
	if len(aesKey) != 32 {
		log.Fatal("AES_KEY must be exactly 32 bytes")
	}

	db, err := sql.Open("sqlite3", "./crossposter.db")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	if err := ensureSchema(db); err != nil {
		log.Fatal(err)
	}

	tmpl := template.Must(template.ParseGlob("ui/templates/*.html"))
	queries := crossposter_db.New(db)
	rootCtx := context.Background()
	jobs := make(chan crossposter_db.PostJob, 100)
	app := &app{
		tmpl:    tmpl,
		db:      db,
		queries: queries,
		aesKey:  aesKey,
		jobs:    jobs,
		rootCtx: rootCtx,
	}

	go workers.StartWorkerPool(rootCtx, queries, jobs, aesKey, app.schedulePendingJob)
	app.recoverPendingJobs(rootCtx)

	http.HandleFunc("/", app.landingHandler)
	http.HandleFunc("/signup", app.signupHandler)
	http.HandleFunc("/login", app.loginHandler)
	http.HandleFunc("/logout", app.authOnly(app.logoutHandler))
	http.HandleFunc("/dashboard", app.authOnly(app.dashboardHandler))
	http.HandleFunc("/add_post", app.authOnly(app.addPostHandler))
	http.HandleFunc("/add_platform", app.authOnly(app.addPlatformHandler))
	http.HandleFunc("/edit_post", app.authOnly(app.editPostHandler))
	http.HandleFunc("/sync/hashnode/edit", app.authOnly(app.syncHashnodeEditHandler))
	http.HandleFunc("/sync/hashnode/delete", app.authOnly(app.syncHashnodeDeleteHandler))
	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("ui/static"))))

	log.Println("Server running on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func ensureSchema(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			username TEXT UNIQUE NOT NULL,
			password_hash TEXT NOT NULL,
			api_key TEXT UNIQUE NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS posts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			title TEXT NOT NULL,
			content TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY(user_id) REFERENCES users(id)
		);`,
		`CREATE TABLE IF NOT EXISTS post_jobs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			post_id INTEGER NOT NULL,
			platform TEXT NOT NULL,
			status TEXT DEFAULT 'pending',
			scheduled_at DATETIME,
			retry_count INTEGER DEFAULT 0,
			last_error TEXT,
			FOREIGN KEY(post_id) REFERENCES posts(id)
		);`,
		`CREATE TABLE IF NOT EXISTS platform_accounts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			platform TEXT NOT NULL,
			token_encrypted TEXT NOT NULL,
			FOREIGN KEY(user_id) REFERENCES users(id),
			UNIQUE(user_id, platform)
		);`,
		`CREATE TABLE IF NOT EXISTS platform_post_refs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			post_id INTEGER NOT NULL,
			platform TEXT NOT NULL,
			remote_id TEXT NOT NULL,
			remote_url TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY(user_id) REFERENCES users(id),
			FOREIGN KEY(post_id) REFERENCES posts(id),
			UNIQUE(post_id, platform)
		);`,
		`CREATE TABLE IF NOT EXISTS post_platform_options (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			post_id INTEGER NOT NULL,
			platform TEXT NOT NULL,
			options_json TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY(user_id) REFERENCES users(id),
			FOREIGN KEY(post_id) REFERENCES posts(id),
			UNIQUE(post_id, platform)
		);`,
		`CREATE TABLE IF NOT EXISTS job_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			job_id INTEGER NOT NULL,
			post_id INTEGER NOT NULL,
			platform TEXT NOT NULL,
			stage TEXT NOT NULL,
			message TEXT NOT NULL,
			details_json TEXT,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY(user_id) REFERENCES users(id),
			FOREIGN KEY(job_id) REFERENCES post_jobs(id),
			FOREIGN KEY(post_id) REFERENCES posts(id)
		);`,
		`CREATE TABLE IF NOT EXISTS ai_threads (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			title TEXT NOT NULL DEFAULT 'Compose Session',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY(user_id) REFERENCES users(id)
		);`,
		`CREATE TABLE IF NOT EXISTS ai_messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			thread_id INTEGER NOT NULL,
			role TEXT NOT NULL,
			content TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY(thread_id) REFERENCES ai_threads(id)
		);`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	if _, err := db.Exec(`ALTER TABLE post_jobs ADD COLUMN scheduled_at DATETIME`); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
		return err
	}
	return nil
}

func (a *app) authOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, err := a.currentUser(r)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

func (a *app) currentUser(r *http.Request) (crossposter_db.User, error) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return crossposter_db.User{}, err
	}
	if cookie.Value == "" {
		return crossposter_db.User{}, errors.New("empty session")
	}
	return a.queries.GetUserByAPIKey(r.Context(), cookie.Value)
}

func (a *app) setSession(w http.ResponseWriter, apiKey string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    apiKey,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   7 * 24 * 60 * 60,
	})
}

func (a *app) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

func (a *app) landingHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	user, err := a.currentUser(r)
	if err == nil {
		rows, qErr := a.queries.ListDashboardJobsByUserLimit(r.Context(), crossposter_db.ListDashboardJobsByUserLimitParams{
			UserID: user.ID,
			Limit:  5,
		})
		if qErr != nil {
			http.Error(w, "failed to load home", http.StatusInternalServerError)
			return
		}
		jobs, stats := toDashboardJobsLimited(rows)
		var latest *DashboardJob
		if len(jobs) > 0 {
			latest = &jobs[0]
		}
		_ = a.tmpl.ExecuteTemplate(w, "home.html", map[string]any{
			"Username": user.Username,
			"Jobs":     jobs,
			"Stats":    stats,
			"Latest":   latest,
		})
		return
	}
	_ = a.tmpl.ExecuteTemplate(w, "index.html", nil)
}

func (a *app) signupHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		_ = a.tmpl.ExecuteTemplate(w, "signup.html", map[string]any{"Error": ""})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	if username == "" || password == "" {
		_ = a.tmpl.ExecuteTemplate(w, "signup.html", map[string]any{"Error": "username and password are required"})
		return
	}

	hashed, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "failed to hash password", http.StatusInternalServerError)
		return
	}

	apiKey, err := generateAPIKey()
	if err != nil {
		http.Error(w, "failed to generate api key", http.StatusInternalServerError)
		return
	}

	user, err := a.queries.CreateUser(r.Context(), crossposter_db.CreateUserParams{
		Username:     username,
		PasswordHash: string(hashed),
		ApiKey:       apiKey,
	})
	if err != nil {
		_ = a.tmpl.ExecuteTemplate(w, "signup.html", map[string]any{"Error": "username already exists"})
		return
	}

	a.setSession(w, user.ApiKey)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *app) loginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		_ = a.tmpl.ExecuteTemplate(w, "login.html", map[string]any{"Error": ""})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	if username == "" || password == "" {
		_ = a.tmpl.ExecuteTemplate(w, "login.html", map[string]any{"Error": "username and password are required"})
		return
	}

	user, err := a.queries.GetUserByUsername(r.Context(), username)
	if err != nil {
		_ = a.tmpl.ExecuteTemplate(w, "login.html", map[string]any{"Error": "invalid credentials"})
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		_ = a.tmpl.ExecuteTemplate(w, "login.html", map[string]any{"Error": "invalid credentials"})
		return
	}

	a.setSession(w, user.ApiKey)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *app) logoutHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.clearSession(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (a *app) dashboardHandler(w http.ResponseWriter, r *http.Request) {
	user, err := a.currentUser(r)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	rows, err := a.queries.ListDashboardJobsByUser(r.Context(), user.ID)
	if err != nil {
		http.Error(w, "failed to load dashboard", http.StatusInternalServerError)
		return
	}

	jobs, stats := toDashboardJobs(rows)
	var latest *DashboardJob
	if len(jobs) > 0 {
		latest = &jobs[0]
	}

	_ = a.tmpl.ExecuteTemplate(w, "dashboard.html", map[string]any{
		"Username": user.Username,
		"Jobs":     jobs,
		"Stats":    stats,
		"Latest":   latest,
	})
}

func (a *app) addPostHandler(w http.ResponseWriter, r *http.Request) {
	user, err := a.currentUser(r)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	if r.Method == http.MethodGet {
		threadID, _ := a.ensureComposeThread(r.Context(), user.ID, r.URL.Query().Get("thread"))
		data := addPostPageData{
			Error:       "",
			Success:     "",
			Intent:      "",
			Title:       "",
			Content:     "",
			AutoSelect:  true,
			ScheduledAt: "",
			ThreadID:    threadID,
		}
		data.ChatHistory = a.loadChatHistory(r.Context(), user.ID, threadID)
		if r.URL.Query().Get("queued") == "1" {
			data.Success = "Post queued successfully."
		}
		if traceID, ok := parsePositiveInt64(r.URL.Query().Get("post_id")); ok {
			data.TracePostID = traceID
			a.fillTraceData(r.Context(), user.ID, &data)
		}
		_ = a.tmpl.ExecuteTemplate(w, "add_post.html", data)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	intent := strings.TrimSpace(r.FormValue("intent"))
	title := strings.TrimSpace(r.FormValue("title"))
	content := strings.TrimSpace(r.FormValue("content"))
	scheduledAt := strings.TrimSpace(r.FormValue("scheduled_at"))
	tzOffset := strings.TrimSpace(r.FormValue("tz_offset_minutes"))
	autoSelect := r.FormValue("auto_select") == "1"
	aiConfirm := r.FormValue("ai_confirm") == "1"
	action := strings.TrimSpace(r.FormValue("action"))
	if qAction := strings.TrimSpace(r.URL.Query().Get("action")); qAction != "" {
		action = qAction
	}
	confirmed := r.FormValue("confirmed") == "1"
	chatInput := strings.TrimSpace(r.FormValue("chat_input"))
	threadID, _ := a.ensureComposeThread(r.Context(), user.ID, r.FormValue("thread_id"))
	pageData := addPostPageData{
		Error:       "",
		Success:     "",
		Intent:      intent,
		Title:       title,
		Content:     content,
		AutoSelect:  autoSelect,
		ScheduledAt: scheduledAt,
		AIConfirm:   aiConfirm,
		Confirmed:   confirmed,
		ThreadID:    threadID,
		ChatInput:   chatInput,
	}
	pageData.ChatHistory = a.loadChatHistory(r.Context(), user.ID, threadID)

	platforms := r.Form["platforms"]
	optionsByPlatform := parsePlatformOptionsFromForm(r)

	if action == "analyze" {
		plan := resolveIntentPlan(r.Context(), intent, content, title)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(plan)
		return
	}

	if action == "chat" {
		if chatInput == "" {
			pageData.Error = "chat message is required"
			_ = a.tmpl.ExecuteTemplate(w, "add_post.html", pageData)
			return
		}
		_, _ = a.queries.CreateAIMessage(r.Context(), crossposter_db.CreateAIMessageParams{
			ThreadID: threadID,
			Role:     "user",
			Content:  chatInput,
		})
		_ = a.queries.TouchAIThread(r.Context(), threadID)
		updatedHistory := a.loadAIConversation(r.Context(), user.ID, threadID)
		assistantReply := a.runComposeAgent(r.Context(), updatedHistory, intent, title, content, platforms, optionsByPlatform)
		if strings.TrimSpace(assistantReply) != "" {
			_, _ = a.queries.CreateAIMessage(r.Context(), crossposter_db.CreateAIMessageParams{
				ThreadID: threadID,
				Role:     "assistant",
				Content:  assistantReply,
			})
			_ = a.queries.TouchAIThread(r.Context(), threadID)
		}
		pageData.ChatInput = ""
		pageData.ChatHistory = a.loadChatHistory(r.Context(), user.ID, threadID)
		_ = a.tmpl.ExecuteTemplate(w, "add_post.html", pageData)
		return
	}

	if content == "" {
		pageData.Error = "content is required"
		_ = a.tmpl.ExecuteTemplate(w, "add_post.html", pageData)
		return
	}
	if title == "" {
		title = generateTitleFromContent(intent, content)
	}
	if title == "" {
		title = "Untitled Post"
	}

	plan := resolveIntentPlan(r.Context(), intent, content, title)
	if autoSelect {
		platforms = plan.Platforms
	}
	if len(platforms) == 0 {
		platforms = plan.Platforms
	}
	if len(platforms) == 0 {
		platforms = append([]string{}, supportedPlatforms...)
	}
	if scheduledAt == "" && plan.Schedule != "" {
		scheduledAt = plan.Schedule
	}
	pageData.Title = title
	if strings.TrimSpace(r.FormValue("title")) == "" && plan.Title != "" {
		title = plan.Title
		pageData.Title = plan.Title
	}
	if includesPlatform(platforms, "hashnode") {
		if len([]rune(title)) < 6 || len([]rune(content)) < 5 {
			pageData.Error = "For Hashnode: title must be at least 6 chars and content at least 5 chars"
			_ = a.tmpl.ExecuteTemplate(w, "add_post.html", pageData)
			return
		}
	}

	if action == "preview" || (aiConfirm && !confirmed) {
		pageData.PreviewMode = true
		pageData.Confirmations = buildAIConfirmations(platforms, optionsByPlatform)
		_ = a.tmpl.ExecuteTemplate(w, "add_post.html", pageData)
		return
	}

	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, "failed to start transaction", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	txQueries := a.queries.WithTx(tx)

	post, err := txQueries.CreatePost(r.Context(), crossposter_db.CreatePostParams{
		UserID:  user.ID,
		Title:   title,
		Content: content,
	})
	if err != nil {
		http.Error(w, "failed to create post", http.StatusInternalServerError)
		return
	}

	createdJobs := make([]crossposter_db.PostJob, 0, len(platforms))
	for _, platform := range platforms {
		if !isSupportedPlatform(platform) {
			continue
		}
		job, err := txQueries.CreatePostJob(r.Context(), crossposter_db.CreatePostJobParams{
			PostID:      post.ID,
			Platform:    platform,
			ScheduledAt: nullableTime(scheduledAt, tzOffset),
		})
		if err != nil {
			http.Error(w, "failed to create jobs", http.StatusInternalServerError)
			return
		}
		details, _ := json.Marshal(map[string]any{
			"scheduled_at":      formatNullTimeToLocal(job.ScheduledAt),
			"auto_selected":     autoSelect,
			"ai_confirmation":   aiConfirm,
			"title_generated":   strings.TrimSpace(r.FormValue("title")) == "",
			"intent_platforms":  plan.Platforms,
			"intent_suggestion": plan.Title,
		})
		_ = txQueries.CreateJobEvent(r.Context(), crossposter_db.CreateJobEventParams{
			UserID:      user.ID,
			JobID:       job.ID,
			PostID:      post.ID,
			Platform:    platform,
			Stage:       "queued",
			Message:     "Job queued for delivery",
			DetailsJson: sql.NullString{String: string(details), Valid: len(details) > 0},
		})

		optionRaw, ok := optionsByPlatform[platform]
		if ok {
			optionJSON, _ := json.Marshal(optionRaw)
			_ = txQueries.UpsertPostPlatformOptions(r.Context(), crossposter_db.UpsertPostPlatformOptionsParams{
				UserID:      user.ID,
				PostID:      post.ID,
				Platform:    platform,
				OptionsJson: string(optionJSON),
			})
		}
		createdJobs = append(createdJobs, job)
	}

	if err := tx.Commit(); err != nil {
		http.Error(w, "failed to save post", http.StatusInternalServerError)
		return
	}
	for _, job := range createdJobs {
		a.schedulePendingJob(job)
	}

	http.Redirect(w, r, "/add_post?queued=1&post_id="+strconv.FormatInt(post.ID, 10)+"&thread="+strconv.FormatInt(threadID, 10), http.StatusSeeOther)
}

func (a *app) addPlatformHandler(w http.ResponseWriter, r *http.Request) {
	user, err := a.currentUser(r)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	if r.Method == http.MethodGet {
		selected := strings.TrimSpace(r.URL.Query().Get("platform"))
		if selected == "" {
			selected = "devto"
		}
		platforms, err := a.getConfiguredPlatforms(r.Context(), user.ID)
		if err != nil {
			http.Error(w, "failed to load platform accounts", http.StatusInternalServerError)
			return
		}
		defaults, _ := a.getPlatformFormDefaults(r.Context(), user.ID, selected)
		_ = a.tmpl.ExecuteTemplate(w, "add_platform.html", map[string]any{
			"Error":               "",
			"Success":             r.URL.Query().Get("saved") == "1",
			"ConfiguredPlatforms": platforms,
			"SelectedPlatform":    selected,
			"DevtoToken":          defaults.Token,
			"HashnodeToken":       defaults.Token,
			"HashnodeHost":        defaults.HashnodeHost,
			"BlueskyIdentifier":   defaults.Identifier,
			"BlueskyAppPassword":  defaults.AppPassword,
			"MediumToken":         defaults.Token,
			"MediumPublicationID": defaults.MediumPublicationID,
			"RedditToken":         defaults.Token,
			"RedditSubreddit":     defaults.RedditSubreddit,
			"XToken":              defaults.Token,
			"SubstackToken":       defaults.Token,
			"SubstackEndpoint":    defaults.SubstackEndpoint,
		})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	platform := strings.TrimSpace(r.FormValue("platform"))
	payloadJSON, formErr := buildPlatformPayload(platform, r)
	if formErr != "" {
		platforms, _ := a.getConfiguredPlatforms(r.Context(), user.ID)
		defaults, _ := a.getPlatformFormDefaults(r.Context(), user.ID, platform)
		_ = a.tmpl.ExecuteTemplate(w, "add_platform.html", map[string]any{
			"Error":               formErr,
			"Success":             false,
			"ConfiguredPlatforms": platforms,
			"SelectedPlatform":    platform,
			"DevtoToken":          defaults.Token,
			"HashnodeToken":       defaults.Token,
			"HashnodeHost":        defaults.HashnodeHost,
			"BlueskyIdentifier":   defaults.Identifier,
			"BlueskyAppPassword":  defaults.AppPassword,
			"MediumToken":         defaults.Token,
			"MediumPublicationID": defaults.MediumPublicationID,
			"RedditToken":         defaults.Token,
			"RedditSubreddit":     defaults.RedditSubreddit,
			"XToken":              defaults.Token,
			"SubstackToken":       defaults.Token,
			"SubstackEndpoint":    defaults.SubstackEndpoint,
		})
		return
	}

	tokenEncrypted, err := auth.EncryptToken(payloadJSON, a.aesKey)
	if err != nil {
		http.Error(w, "failed to encrypt token", http.StatusInternalServerError)
		return
	}

	err = a.queries.UpsertPlatformAccount(r.Context(), crossposter_db.UpsertPlatformAccountParams{
		UserID:         user.ID,
		Platform:       platform,
		TokenEncrypted: tokenEncrypted,
	})
	if err != nil {
		http.Error(w, "failed to save platform token", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/add_platform?saved=1&platform="+url.QueryEscape(platform), http.StatusSeeOther)
}

func (a *app) editPostHandler(w http.ResponseWriter, r *http.Request) {
	user, err := a.currentUser(r)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	postID := strings.TrimSpace(r.URL.Query().Get("id"))
	if postID == "" {
		http.Error(w, "missing post id", http.StatusBadRequest)
		return
	}

	postNumericID, convErr := strconv.ParseInt(postID, 10, 64)
	if convErr != nil {
		http.Error(w, "invalid post id", http.StatusBadRequest)
		return
	}
	postRow, err := a.queries.GetPostByIDForUser(r.Context(), crossposter_db.GetPostByIDForUserParams{
		ID:     postNumericID,
		UserID: user.ID,
	})
	if err != nil {
		http.Error(w, "post not found", http.StatusNotFound)
		return
	}
	post := EditablePost{ID: postRow.ID, Title: postRow.Title, Content: postRow.Content}

	if r.Method == http.MethodGet {
		hashnodeRef, _ := a.getRemoteRef(r.Context(), user.ID, post.ID, "hashnode")
		_ = a.tmpl.ExecuteTemplate(w, "edit_post.html", map[string]any{
			"Error":             "",
			"Success":           r.URL.Query().Get("saved") == "1",
			"Post":              post,
			"HashnodeRemoteID":  hashnodeRef.RemoteID,
			"HashnodeRemoteURL": hashnodeRef.RemoteURL,
			"SyncMessage":       r.URL.Query().Get("sync"),
			"SyncError":         r.URL.Query().Get("sync_error"),
		})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	title := strings.TrimSpace(r.FormValue("title"))
	content := strings.TrimSpace(r.FormValue("content"))
	if title == "" || content == "" {
		_ = a.tmpl.ExecuteTemplate(w, "edit_post.html", map[string]any{
			"Error":   "title and content are required",
			"Success": false,
			"Post":    post,
		})
		return
	}

	err = a.queries.UpdatePostForUser(r.Context(), crossposter_db.UpdatePostForUserParams{
		Title:   title,
		Content: content,
		ID:      post.ID,
		UserID:  user.ID,
	})
	if err != nil {
		http.Error(w, "failed to update post", http.StatusInternalServerError)
		return
	}

	requeue := r.FormValue("requeue") == "1"
	if requeue {
		platforms := r.Form["platforms"]
		if len(platforms) == 0 {
			platforms = append([]string{}, supportedPlatforms...)
		}
		if includesPlatform(platforms, "hashnode") {
			if len([]rune(title)) < 6 || len([]rune(content)) < 5 {
				_ = a.tmpl.ExecuteTemplate(w, "edit_post.html", map[string]any{
					"Error":   "For Hashnode re-publish: title must be at least 6 chars and content at least 5 chars",
					"Success": false,
					"Post": EditablePost{
						ID:      post.ID,
						Title:   title,
						Content: content,
					},
				})
				return
			}
		}
		for _, platform := range platforms {
			if !isSupportedPlatform(platform) {
				continue
			}
			job, jobErr := a.queries.CreatePostJob(r.Context(), crossposter_db.CreatePostJobParams{
				PostID:      post.ID,
				Platform:    platform,
				ScheduledAt: sql.NullTime{},
			})
			if jobErr == nil {
				a.schedulePendingJob(job)
			}
		}
	}

	http.Redirect(w, r, "/edit_post?id="+postID+"&saved=1", http.StatusSeeOther)
}

func (a *app) syncHashnodeEditHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, err := a.currentUser(r)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	postID := strings.TrimSpace(r.FormValue("post_id"))
	if postID == "" {
		http.Error(w, "missing post id", http.StatusBadRequest)
		return
	}

	postNumericID, convErr := strconv.ParseInt(postID, 10, 64)
	if convErr != nil {
		http.Redirect(w, r, "/edit_post?id="+url.QueryEscape(postID)+"&sync_error="+url.QueryEscape("invalid post id"), http.StatusSeeOther)
		return
	}
	postRow, err := a.queries.GetPostByIDForUser(r.Context(), crossposter_db.GetPostByIDForUserParams{
		ID:     postNumericID,
		UserID: user.ID,
	})
	if err != nil {
		http.Redirect(w, r, "/edit_post?id="+url.QueryEscape(postID)+"&sync_error="+url.QueryEscape("post not found"), http.StatusSeeOther)
		return
	}
	post := EditablePost{ID: postRow.ID, Title: postRow.Title, Content: postRow.Content}

	ref, err := a.getRemoteRef(r.Context(), user.ID, post.ID, "hashnode")
	if err != nil || ref.RemoteID == "" {
		http.Redirect(w, r, "/edit_post?id="+url.QueryEscape(postID)+"&sync_error="+url.QueryEscape("hashnode remote post not linked yet"), http.StatusSeeOther)
		return
	}

	creds, err := a.getPlatformFormDefaults(r.Context(), user.ID, "hashnode")
	if err != nil || creds.Token == "" || creds.HashnodeHost == "" {
		http.Redirect(w, r, "/edit_post?id="+url.QueryEscape(postID)+"&sync_error="+url.QueryEscape("missing hashnode credentials"), http.StatusSeeOther)
		return
	}

	if len([]rune(post.Title)) < 6 || len([]rune(post.Content)) < 5 {
		http.Redirect(w, r, "/edit_post?id="+url.QueryEscape(postID)+"&sync_error="+url.QueryEscape("hashnode requires title >= 6 and content >= 5"), http.StatusSeeOther)
		return
	}

	remote, err := platforms.UpdateHashnodePost(ref.RemoteID, post.Title, post.Content, creds.Token, creds.HashnodeHost)
	if err != nil {
		http.Redirect(w, r, "/edit_post?id="+url.QueryEscape(postID)+"&sync_error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}

	if remote.ID != "" {
		_ = a.queries.UpsertPlatformPostRef(r.Context(), crossposter_db.UpsertPlatformPostRefParams{
			UserID:   user.ID,
			PostID:   post.ID,
			Platform: "hashnode",
			RemoteID: remote.ID,
			RemoteUrl: sql.NullString{
				String: remote.URL,
				Valid:  remote.URL != "",
			},
		})
	}
	http.Redirect(w, r, "/edit_post?id="+url.QueryEscape(postID)+"&sync="+url.QueryEscape("hashnode updated"), http.StatusSeeOther)
}

func (a *app) syncHashnodeDeleteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, err := a.currentUser(r)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	postID := strings.TrimSpace(r.FormValue("post_id"))
	if postID == "" {
		http.Error(w, "missing post id", http.StatusBadRequest)
		return
	}

	localPostID, convErr := strconv.ParseInt(postID, 10, 64)
	if convErr != nil {
		http.Redirect(w, r, "/edit_post?id="+url.QueryEscape(postID)+"&sync_error="+url.QueryEscape("invalid post id"), http.StatusSeeOther)
		return
	}
	_, err = a.queries.GetPostByIDForUser(r.Context(), crossposter_db.GetPostByIDForUserParams{
		ID:     localPostID,
		UserID: user.ID,
	})
	if err != nil {
		http.Redirect(w, r, "/edit_post?id="+url.QueryEscape(postID)+"&sync_error="+url.QueryEscape("post not found"), http.StatusSeeOther)
		return
	}

	ref, err := a.getRemoteRef(r.Context(), user.ID, localPostID, "hashnode")
	if err != nil || ref.RemoteID == "" {
		http.Redirect(w, r, "/edit_post?id="+url.QueryEscape(postID)+"&sync_error="+url.QueryEscape("hashnode remote post not linked"), http.StatusSeeOther)
		return
	}

	creds, err := a.getPlatformFormDefaults(r.Context(), user.ID, "hashnode")
	if err != nil || creds.Token == "" {
		http.Redirect(w, r, "/edit_post?id="+url.QueryEscape(postID)+"&sync_error="+url.QueryEscape("missing hashnode token"), http.StatusSeeOther)
		return
	}

	if err := platforms.RemoveHashnodePost(ref.RemoteID, creds.Token); err != nil {
		http.Redirect(w, r, "/edit_post?id="+url.QueryEscape(postID)+"&sync_error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}

	_ = a.queries.DeleteHashnodePostRef(r.Context(), crossposter_db.DeleteHashnodePostRefParams{
		PostID: localPostID,
		UserID: user.ID,
	})
	http.Redirect(w, r, "/edit_post?id="+url.QueryEscape(postID)+"&sync="+url.QueryEscape("hashnode deleted"), http.StatusSeeOther)
}

func (a *app) getConfiguredPlatforms(ctx context.Context, userID int64) ([]ConfiguredPlatform, error) {
	rows, err := a.queries.ListConfiguredPlatforms(ctx, userID)
	if err != nil {
		return nil, err
	}

	items := make([]ConfiguredPlatform, 0)
	for _, platform := range rows {
		item := ConfiguredPlatform{Platform: platform}
		item.AuthType = platformAuthType(item.Platform)
		items = append(items, item)
	}
	return items, nil
}

func (a *app) getRemoteRef(ctx context.Context, userID, postID int64, platform string) (RemoteRef, error) {
	ref, err := a.queries.GetPlatformPostRef(ctx, crossposter_db.GetPlatformPostRefParams{
		UserID:   userID,
		PostID:   postID,
		Platform: platform,
	})
	if err != nil {
		return RemoteRef{}, err
	}
	return RemoteRef{
		RemoteID:  ref.RemoteID,
		RemoteURL: ref.RemoteUrl,
	}, nil
}

func platformAuthType(platform string) string {
	switch platform {
	case "devto":
		return "API key token"
	case "hashnode":
		return "Personal access token + blog host"
	case "bluesky":
		return "Identifier + app password"
	case "medium":
		return "Integration token + publication id"
	case "reddit":
		return "OAuth token + subreddit"
	case "x":
		return "OAuth bearer token"
	case "substack":
		return "Automation endpoint + token"
	default:
		return "Custom credential"
	}
}

func buildPlatformPayload(platform string, r *http.Request) (string, string) {
	switch platform {
	case "devto":
		token := strings.TrimSpace(r.FormValue("devto_token"))
		if token == "" {
			return "", "Dev.to token is required"
		}
		payload, _ := json.Marshal(PlatformCredentialPayload{
			Kind:  "devto_token",
			Token: token,
		})
		return string(payload), ""
	case "hashnode":
		token := strings.TrimSpace(r.FormValue("hashnode_token"))
		host := normalizeHashnodeHost(strings.TrimSpace(r.FormValue("hashnode_host")))
		if token == "" || host == "" {
			return "", "Hashnode token and blog host are required"
		}
		payload, _ := json.Marshal(PlatformCredentialPayload{
			Kind:         "hashnode_token",
			Token:        token,
			HashnodeHost: host,
		})
		return string(payload), ""
	case "bluesky":
		identifier := strings.TrimSpace(r.FormValue("bluesky_identifier"))
		appPassword := strings.TrimSpace(r.FormValue("bluesky_app_password"))
		if identifier == "" || appPassword == "" {
			return "", "Bluesky identifier and app password are required"
		}
		payload, _ := json.Marshal(PlatformCredentialPayload{
			Kind:        "bluesky_app_password",
			Identifier:  identifier,
			AppPassword: appPassword,
		})
		return string(payload), ""
	case "medium":
		token := strings.TrimSpace(r.FormValue("medium_token"))
		publicationID := strings.TrimSpace(r.FormValue("medium_publication_id"))
		if token == "" || publicationID == "" {
			return "", "Medium token and publication id are required"
		}
		payload, _ := json.Marshal(PlatformCredentialPayload{
			Kind:                "medium_token",
			Token:               token,
			MediumPublicationID: publicationID,
		})
		return string(payload), ""
	case "reddit":
		token := strings.TrimSpace(r.FormValue("reddit_token"))
		subreddit := strings.TrimSpace(r.FormValue("reddit_subreddit"))
		if token == "" || subreddit == "" {
			return "", "Reddit token and subreddit are required"
		}
		subreddit = strings.TrimPrefix(strings.ToLower(subreddit), "r/")
		payload, _ := json.Marshal(PlatformCredentialPayload{
			Kind:            "reddit_oauth",
			Token:           token,
			RedditSubreddit: subreddit,
		})
		return string(payload), ""
	case "x":
		token := strings.TrimSpace(r.FormValue("x_token"))
		if token == "" {
			return "", "X bearer token is required"
		}
		payload, _ := json.Marshal(PlatformCredentialPayload{
			Kind:  "x_bearer",
			Token: token,
		})
		return string(payload), ""
	case "substack":
		endpoint := strings.TrimSpace(r.FormValue("substack_endpoint"))
		token := strings.TrimSpace(r.FormValue("substack_token"))
		if endpoint == "" || token == "" {
			return "", "Substack endpoint and token are required"
		}
		payload, _ := json.Marshal(PlatformCredentialPayload{
			Kind:             "substack_endpoint",
			Token:            token,
			SubstackEndpoint: endpoint,
		})
		return string(payload), ""
	default:
		return "", "unsupported platform"
	}
}

func normalizeHashnodeHost(raw string) string {
	host := strings.TrimSpace(raw)
	if host == "" {
		return ""
	}
	if strings.Contains(host, "://") {
		u, err := url.Parse(host)
		if err == nil && u.Host != "" {
			return strings.ToLower(strings.TrimSpace(u.Host))
		}
	}
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	if idx := strings.Index(host, "/"); idx >= 0 {
		host = host[:idx]
	}
	return strings.ToLower(strings.TrimSpace(host))
}

func includesPlatform(platforms []string, target string) bool {
	for _, p := range platforms {
		if p == target {
			return true
		}
	}
	return false
}

func isSupportedPlatform(platform string) bool {
	for _, p := range supportedPlatforms {
		if p == platform {
			return true
		}
	}
	return false
}

func nullableTime(input, tzOffsetMinutes string) sql.NullTime {
	v := strings.TrimSpace(input)
	if v == "" {
		return sql.NullTime{}
	}

	layouts := []string{
		"2006-01-02T15:04",
		"2006-01-02T15:04:05",
		"2006-01-02T15:04:05.000",
	}
	loc := time.Local
	if tzOffsetMinutes != "" {
		if offset, err := strconv.Atoi(tzOffsetMinutes); err == nil {
			loc = time.FixedZone("user", -offset*60)
		}
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, v, loc); err == nil {
			return sql.NullTime{Time: t.UTC(), Valid: true}
		}
	}
	return sql.NullTime{}
}

func formatNullTimeToLocal(raw sql.NullTime) string {
	if !raw.Valid {
		return ""
	}
	return raw.Time.In(time.Local).Format("2006-01-02 15:04 MST")
}

func inferPlatformsFromIntent(intent string) []string {
	norm := strings.ToLower(strings.TrimSpace(intent))
	if norm == "" {
		return nil
	}

	if strings.Contains(norm, "all platforms") || strings.Contains(norm, "everywhere") || strings.Contains(norm, "all") {
		return append([]string{}, supportedPlatforms...)
	}

	platforms := make([]string, 0, 3)
	if strings.Contains(norm, "dev.to") || strings.Contains(norm, "devto") || strings.Contains(norm, "developer article") {
		platforms = append(platforms, "devto")
	}
	if strings.Contains(norm, "hashnode") || strings.Contains(norm, "engineering blog") || strings.Contains(norm, "tech blog") {
		platforms = append(platforms, "hashnode")
	}
	if strings.Contains(norm, "bluesky") || strings.Contains(norm, "bsky") || strings.Contains(norm, "thread") || strings.Contains(norm, "social") {
		platforms = append(platforms, "bluesky")
	}
	if strings.Contains(norm, "x") || strings.Contains(norm, "twitter") || strings.Contains(norm, "tweet") {
		platforms = append(platforms, "x")
	}
	if strings.Contains(norm, "reddit") || strings.Contains(norm, "subreddit") || strings.Contains(norm, "community") {
		platforms = append(platforms, "reddit")
	}
	if strings.Contains(norm, "medium") {
		platforms = append(platforms, "medium")
	}
	if strings.Contains(norm, "substack") || strings.Contains(norm, "newsletter") {
		platforms = append(platforms, "substack")
	}
	return uniquePlatforms(platforms)
}

func resolveIntentPlan(ctx context.Context, intent, content, currentTitle string) intentai.Plan {
	platforms := inferPlatformsFromIntent(intent)
	title := currentTitle
	if title == "" {
		title = generateTitleFromContent(intent, content)
	}

	metaCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	client := intentai.NewClient(25 * time.Second)
	plan, err := client.Infer(metaCtx, intent, content, title)
	if err != nil {
		return intentai.Plan{
			Platforms: filterSupported(platforms),
			Title:     title,
			Schedule:  deriveScheduleFromIntent(intent),
		}
	}

	plan.Platforms = filterSupported(plan.Platforms)
	if plan.Title == "" {
		plan.Title = title
	}
	if plan.Schedule == "" {
		plan.Schedule = deriveScheduleFromIntent(intent)
	}
	return plan
}

func deriveScheduleFromIntent(intent string) string {
	if schedule, ok := parseIntentSchedule(intent); ok {
		return schedule.UTC().Format(time.RFC3339)
	}
	return ""
}

var scheduleRegex = regexp.MustCompile(`(?i)\bat\b\s*` +
	`(?P<hour>\d{1,2})` +
	`(?::(?P<minute>\d{2}))?` +
	`(?:\s*(?P<ampm>am|pm))?` +
	`(?:\s*(?P<tz>[A-Za-z/]+))?`)

var timezoneAliases = map[string]string{
	"utc": "UTC",
	"gmt": "UTC",
	"ist": "Asia/Kolkata",
	"edt": "America/New_York",
	"est": "America/New_York",
	"cdt": "America/Chicago",
	"cst": "America/Chicago",
	"pst": "America/Los_Angeles",
	"pdt": "America/Los_Angeles",
	"cet": "Europe/Paris",
	"cest": "Europe/Paris",
}

func parseIntentSchedule(intent string) (time.Time, bool) {
	matches := scheduleRegex.FindStringSubmatch(intent)
	if len(matches) == 0 {
		return time.Time{}, false
	}
	m := make(map[string]string)
	for i, name := range scheduleRegex.SubexpNames() {
		if i == 0 || name == "" {
			continue
		}
		m[name] = matches[i]
	}

	hour, err := strconv.Atoi(m["hour"])
	if err != nil {
		return time.Time{}, false
	}
	minute := 0
	if v := m["minute"]; v != "" {
		minute, _ = strconv.Atoi(v)
	}
	if ampm := strings.ToLower(m["ampm"]); ampm != "" {
		if ampm == "pm" && hour < 12 {
			hour += 12
		}
		if ampm == "am" && hour == 12 {
			hour = 0
		}
	}

	loc := time.Local
	if tz := strings.ToLower(strings.TrimSpace(m["tz"])); tz != "" {
		if alias, ok := timezoneAliases[tz]; ok {
			if l, err := time.LoadLocation(alias); err == nil {
				loc = l
			}
		} else if l, err := time.LoadLocation(strings.ToUpper(tz)); err == nil {
			loc = l
		}
	}

	now := time.Now().In(loc)
	scheduled := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, loc)
	if scheduled.Before(now) {
		scheduled = scheduled.Add(24 * time.Hour)
	}
	return scheduled, true
}

func filterSupported(items []string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		if isSupportedPlatform(item) {
			out = append(out, item)
		}
	}
	return uniquePlatforms(out)
}

func uniquePlatforms(items []string) []string {
	seen := make(map[string]bool, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

func generateTitleFromContent(intent, content string) string {
	base := strings.TrimSpace(intent)
	if base == "" {
		base = strings.TrimSpace(content)
	}
	if base == "" {
		return ""
	}
	base = strings.ReplaceAll(base, "\n", " ")
	words := strings.Fields(base)
	if len(words) == 0 {
		return ""
	}
	if len(words) > 10 {
		words = words[:10]
	}
	return strings.TrimSpace(strings.Join(words, " "))
}

func (a *app) getPlatformFormDefaults(ctx context.Context, userID int64, platform string) (PlatformCredentialPayload, error) {
	if platform == "" {
		return PlatformCredentialPayload{}, nil
	}
	account, err := a.queries.GetPlatformAccount(ctx, crossposter_db.GetPlatformAccountParams{
		UserID:   userID,
		Platform: platform,
	})
	if err != nil {
		return PlatformCredentialPayload{}, err
	}
	plain, err := auth.DecryptToken(account.TokenEncrypted, a.aesKey)
	if err != nil {
		return PlatformCredentialPayload{}, err
	}

	var payload PlatformCredentialPayload
	if err := json.Unmarshal([]byte(plain), &payload); err != nil {
		payload.Token = plain
	}
	return payload, nil
}

func (a *app) ensureComposeThread(ctx context.Context, userID int64, rawID string) (int64, error) {
	if id, ok := parsePositiveInt64(rawID); ok {
		if _, err := a.queries.GetAIThreadByIDForUser(ctx, crossposter_db.GetAIThreadByIDForUserParams{
			ID:     id,
			UserID: userID,
		}); err == nil {
			return id, nil
		}
	}
	thread, err := a.queries.CreateAIThread(ctx, crossposter_db.CreateAIThreadParams{
		UserID: userID,
		Title:  "Compose Session",
	})
	if err != nil {
		return 0, err
	}
	return thread.ID, nil
}

func (a *app) loadAIConversation(ctx context.Context, userID, threadID int64) []intentai.ChatMessage {
	rows, err := a.queries.ListAIMessagesByThread(ctx, crossposter_db.ListAIMessagesByThreadParams{
		ThreadID: threadID,
		UserID:   userID,
	})
	if err != nil {
		return nil
	}
	out := make([]intentai.ChatMessage, 0, len(rows))
	for _, row := range rows {
		out = append(out, intentai.ChatMessage{
			Role:    row.Role,
			Content: row.Content,
		})
	}
	return out
}

func (a *app) loadChatHistory(ctx context.Context, userID, threadID int64) []ChatMessageView {
	rows, err := a.queries.ListAIMessagesByThread(ctx, crossposter_db.ListAIMessagesByThreadParams{
		ThreadID: threadID,
		UserID:   userID,
	})
	if err != nil {
		return nil
	}
	out := make([]ChatMessageView, 0, len(rows))
	for _, row := range rows {
		out = append(out, ChatMessageView{
			Role:      row.Role,
			Content:   row.Content,
			CreatedAt: formatNullTimeToLocal(row.CreatedAt),
		})
	}
	return out
}

func (a *app) runComposeAgent(ctx context.Context, history []intentai.ChatMessage, intent, title, content string, platforms []string, opts map[string]PlatformOptionInput) string {
	optionsJSON, _ := json.Marshal(opts)
	draftContext := fmt.Sprintf(
		"Intent: %s\nTitle: %s\nContent:\n%s\nSelectedPlatforms: %s\nPlatformOptionsJSON: %s",
		strings.TrimSpace(intent),
		strings.TrimSpace(title),
		strings.TrimSpace(content),
		strings.Join(platforms, ","),
		string(optionsJSON),
	)
	client := intentai.NewClient(20 * time.Second)
	resp, err := client.Chat(ctx, history, draftContext)
	if err != nil {
		return "Agent could not respond right now. Please retry."
	}
	return resp
}

func toDashboardJobs(rows []crossposter_db.ListDashboardJobsByUserRow) ([]DashboardJob, DashboardStats) {
	jobs := make([]DashboardJob, 0, len(rows))
	stats := DashboardStats{}
	for _, row := range rows {
		j := dashboardJobFromCommon(row.PostID, row.Title, row.Platform, row.Status, row.CreatedAt, row.ScheduledAt, row.LastError, row.RemoteID, row.RemoteUrl)
		stats = accumulateStats(stats, j.Status)
		jobs = append(jobs, j)
	}
	return jobs, stats
}

func toDashboardJobsLimited(rows []crossposter_db.ListDashboardJobsByUserLimitRow) ([]DashboardJob, DashboardStats) {
	jobs := make([]DashboardJob, 0, len(rows))
	stats := DashboardStats{}
	for _, row := range rows {
		j := dashboardJobFromCommon(row.PostID, row.Title, row.Platform, row.Status, row.CreatedAt, row.ScheduledAt, row.LastError, row.RemoteID, row.RemoteUrl)
		stats = accumulateStats(stats, j.Status)
		jobs = append(jobs, j)
	}
	return jobs, stats
}

func toDashboardJobsForPost(rows []crossposter_db.ListDashboardJobsByPostRow) []DashboardJob {
	jobs := make([]DashboardJob, 0, len(rows))
	for _, row := range rows {
		j := dashboardJobFromCommon(row.PostID, row.Title, row.Platform, row.Status, row.CreatedAt, row.ScheduledAt, row.LastError, row.RemoteID, row.RemoteUrl)
		jobs = append(jobs, j)
	}
	return jobs
}

func dashboardJobFromCommon(postID int64, title, platform string, status sql.NullString, createdAt, scheduledAt sql.NullTime, lastError, remoteID, remoteURL sql.NullString) DashboardJob {
	j := DashboardJob{
		PostID:      postID,
		Title:       title,
		Platform:    platform,
		Status:      "pending",
		CreatedAt:   formatNullTimeToLocal(createdAt),
		ScheduledAt: formatNullTimeToLocal(scheduledAt),
	}
	if status.Valid && status.String != "" {
		j.Status = status.String
	}
	if lastError.Valid {
		j.LastError = lastError.String
	}
	if remoteID.Valid {
		j.RemoteID = remoteID.String
	}
	if remoteURL.Valid {
		j.RemoteURL = remoteURL.String
	}
	return j
}

func accumulateStats(stats DashboardStats, status string) DashboardStats {
	switch status {
	case "success":
		stats.Success++
	case "failed":
		stats.Failed++
	case "processing":
		stats.Processing++
	default:
		stats.Pending++
	}
	return stats
}

func parsePositiveInt64(raw string) (int64, bool) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return 0, false
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func parsePlatformOptionsFromForm(r *http.Request) map[string]PlatformOptionInput {
	out := make(map[string]PlatformOptionInput)
	for _, platform := range supportedPlatforms {
		tags := splitCSV(r.FormValue(platform + "_tags"))
		option := PlatformOptionInput{
			Tags:         tags,
			Series:       strings.TrimSpace(r.FormValue(platform + "_series")),
			PublishMode:  strings.TrimSpace(strings.ToLower(r.FormValue(platform + "_publish_mode"))),
			CanonicalURL: strings.TrimSpace(r.FormValue(platform + "_canonical_url")),
			Subreddit:    strings.TrimPrefix(strings.ToLower(strings.TrimSpace(r.FormValue(platform+"_subreddit"))), "r/"),
			Tone:         strings.TrimSpace(r.FormValue(platform + "_tone")),
			Audience:     strings.TrimSpace(r.FormValue(platform + "_audience")),
			CallToAction: strings.TrimSpace(r.FormValue(platform + "_cta")),
			ThreadMode:   strings.TrimSpace(strings.ToLower(r.FormValue(platform + "_thread_mode"))),
			Additional:   strings.TrimSpace(r.FormValue(platform + "_additional")),
		}
		if option.PublishMode == "" {
			option.PublishMode = "publish"
		}
		if option.ThreadMode == "" {
			option.ThreadMode = "auto"
		}
		if isEmptyPlatformOption(option) {
			continue
		}
		out[platform] = option
	}
	return out
}

func splitCSV(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		p := strings.TrimSpace(part)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func isEmptyPlatformOption(opt PlatformOptionInput) bool {
	return len(opt.Tags) == 0 &&
		opt.Series == "" &&
		opt.CanonicalURL == "" &&
		opt.Subreddit == "" &&
		opt.Tone == "" &&
		opt.Audience == "" &&
		opt.CallToAction == "" &&
		(opt.ThreadMode == "" || opt.ThreadMode == "auto") &&
		opt.Additional == "" &&
		(opt.PublishMode == "" || opt.PublishMode == "publish")
}

func buildAIConfirmations(platforms []string, options map[string]PlatformOptionInput) []AIConfirmation {
	items := make([]AIConfirmation, 0, len(platforms))
	for _, platform := range platforms {
		opt := options[platform]
		note := "ready"
		switch platform {
		case "devto":
			note = "Check tags/series and publish mode for Dev.to formatting."
		case "medium":
			note = "Confirm publish mode and canonical URL for Medium."
		case "reddit":
			note = "Confirm subreddit target and title tone for Reddit rules."
		case "x", "bluesky":
			note = "Short-form adaptation will be generated automatically."
		case "hashnode":
			note = "Requires title >= 6 chars and content >= 5 chars."
		case "substack":
			note = "Will submit full content to configured Substack endpoint."
		}
		if opt.PublishMode == "draft" {
			note += " AI detected draft mode."
		}
		if opt.Tone != "" || opt.Audience != "" || opt.CallToAction != "" {
			note += " Preference hints detected (tone/audience/cta)."
		}
		items = append(items, AIConfirmation{Platform: platform, Note: note})
	}
	return items
}

func (a *app) fillTraceData(ctx context.Context, userID int64, data *addPostPageData) {
	if data.TracePostID == 0 {
		return
	}
	rows, err := a.queries.ListDashboardJobsByPost(ctx, crossposter_db.ListDashboardJobsByPostParams{
		UserID: userID,
		PostID: data.TracePostID,
	})
	if err == nil {
		data.TraceJobs = toDashboardJobsForPost(rows)
	}
	events, err := a.queries.ListJobEventsByPost(ctx, crossposter_db.ListJobEventsByPostParams{
		UserID: userID,
		PostID: data.TracePostID,
	})
	if err == nil {
		data.TraceEvents = make([]JobEventView, 0, len(events))
		for _, ev := range events {
			data.TraceEvents = append(data.TraceEvents, JobEventView{
				Platform:  ev.Platform,
				Stage:     ev.Stage,
				Message:   ev.Message,
				CreatedAt: formatNullTimeToLocal(ev.CreatedAt),
			})
		}
	}
}

func (a *app) recoverPendingJobs(ctx context.Context) {
	pending, err := a.queries.GetPendingJobs(ctx)
	if err != nil {
		log.Println("failed to load pending jobs:", err)
		return
	}
	for _, job := range pending {
		a.schedulePendingJob(job)
	}
}

func (a *app) schedulePendingJob(job crossposter_db.PostJob) {
	dispatch := func() {
		if err := a.claimAndDispatchJob(job); err != nil {
			log.Println("failed to dispatch job:", err)
		}
	}
	if job.ScheduledAt.Valid {
		delay := time.Until(job.ScheduledAt.Time)
		if delay > 0 {
			time.AfterFunc(delay, dispatch)
			return
		}
	}
	go dispatch()
}

func (a *app) claimAndDispatchJob(job crossposter_db.PostJob) error {
	rows, err := a.queries.ClaimPendingJob(a.rootCtx, job.ID)
	if err != nil {
		return err
	}
	if rows == 0 {
		return nil
	}
	job.Status = sql.NullString{String: "processing", Valid: true}
	if post, pErr := a.queries.GetPostByID(a.rootCtx, job.PostID); pErr == nil {
		_ = a.queries.CreateJobEvent(a.rootCtx, crossposter_db.CreateJobEventParams{
			UserID:      post.UserID,
			JobID:       job.ID,
			PostID:      job.PostID,
			Platform:    job.Platform,
			Stage:       "claimed",
			Message:     "Job claimed by dispatcher",
			DetailsJson: sql.NullString{},
		})
	}
	a.jobs <- job
	return nil
}

func generateAPIKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
