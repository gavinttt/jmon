package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"jmon/internal/analyzer"
	"jmon/internal/collector"
	"jmon/internal/storage"
)

//go:embed static/*
var staticFiles embed.FS

// Server is the web server for the jmon UI and API
type Server struct {
	db       *storage.DB
	port     int
	username string
	password string
	sessions sync.Map // token -> expiry time
	server   *http.Server
}

// NewServer creates a new web server
func NewServer(db *storage.DB, port int, username, password string) *Server {
	return &Server{db: db, port: port, username: username, password: password}
}

// Start starts the HTTP server
func (s *Server) Start() error {
	mux := http.NewServeMux()

	// Public routes (no auth)
	mux.HandleFunc("/login", s.handleLoginPage)
	mux.HandleFunc("/api/login", s.handleLogin)

	// API routes (auth required)
	mux.HandleFunc("/api/processes", s.handleProcesses)
	mux.HandleFunc("/api/process/", s.handleProcessAPI)

	// UI routes (auth required)
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/dashboard", s.handleDashboard)
	mux.HandleFunc("/process/", s.handleProcessPage)

	s.server = &http.Server{
		Addr:    fmt.Sprintf(":%d", s.port),
		Handler: s.authMiddleware(mux),
	}
	return s.server.ListenAndServe()
}

// authMiddleware checks session cookie, redirects to /login if not authenticated
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No auth configured, pass through
		if s.username == "" && s.password == "" {
			next.ServeHTTP(w, r)
			return
		}

		path := r.URL.Path
		// Public paths always accessible
		if path == "/login" || path == "/api/login" {
			next.ServeHTTP(w, r)
			return
		}

		// Check session cookie
		cookie, err := r.Cookie("jmon_session")
		if err == nil {
			if expiry, ok := s.sessions.Load(cookie.Value); ok {
				if expiry.(time.Time).After(time.Now()) {
					next.ServeHTTP(w, r)
					return
				}
				s.sessions.Delete(cookie.Value)
			}
		}

		// Not authenticated
		if strings.HasPrefix(path, "/api/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
		} else {
			http.Redirect(w, r, "/login", http.StatusFound)
		}
	})
}

// Stop gracefully stops the server
func (s *Server) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.server.Shutdown(ctx)
}

// --- Auth Handlers ---

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data, err := staticFiles.ReadFile("static/login.html")
	if err != nil {
		http.Error(w, "login page not found", 500)
		return
	}
	w.Write(data)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		// Return auth status
		cookie, err := r.Cookie("jmon_session")
		if err == nil {
			if expiry, ok := s.sessions.Load(cookie.Value); ok {
				if expiry.(time.Time).After(time.Now()) {
					jsonResponse(w, map[string]bool{"authenticated": true})
					return
				}
			}
		}
		w.WriteHeader(http.StatusUnauthorized)
		jsonResponse(w, map[string]bool{"authenticated": false})
		return
	}

	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}

	var creds struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
		jsonError(w, "invalid request", 400)
		return
	}

	if subtle.ConstantTimeCompare([]byte(creds.Username), []byte(s.username)) != 1 ||
		subtle.ConstantTimeCompare([]byte(creds.Password), []byte(s.password)) != 1 {
		jsonError(w, "invalid credentials", 401)
		return
	}

	// Generate session token
	tokenBytes := make([]byte, 32)
	rand.Read(tokenBytes)
	token := hex.EncodeToString(tokenBytes)
	expiry := time.Now().Add(24 * time.Hour)
	s.sessions.Store(token, expiry)

	http.SetCookie(w, &http.Cookie{
		Name:     "jmon_session",
		Value:    token,
		Path:     "/",
		Expires:  expiry,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})

	jsonResponse(w, map[string]bool{"authenticated": true})
}

// --- API Handlers ---

func (s *Server) handleProcesses(w http.ResponseWriter, r *http.Request) {
	procs, err := collector.CollectJavaProcesses()
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	jsonResponse(w, procs)
}

func (s *Server) handleProcessAPI(w http.ResponseWriter, r *http.Request) {
	// Parse: /api/process/{pid}/{resource}
	path := strings.TrimPrefix(r.URL.Path, "/api/process/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 2 {
		jsonError(w, "invalid path", 400)
		return
	}

	pid, err := strconv.Atoi(parts[0])
	if err != nil {
		jsonError(w, "invalid pid", 400)
		return
	}

	resource := parts[1]
	from, to := parseTimeRange(r)

	switch resource {
	case "memory":
		data, err := s.db.QueryMemoryHistory(pid, from, to)
		if err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
		jsonResponse(w, data)

	case "cpu":
		data, err := s.db.QueryCPUHistory(pid, from, to)
		if err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
		jsonResponse(w, data)

	case "threads":
		top := queryInt(r, "top", 50)
		data, err := s.db.QueryLatestThreads(pid, top)
		if err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
		jsonResponse(w, data)

	case "heap":
		top := queryInt(r, "top", 50)
		data, err := s.db.QueryLatestHeapHisto(pid, top)
		if err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
		jsonResponse(w, data)

	case "leak":
		data, err := analyzer.DetectMemoryLeaks(s.db, pid, from, to)
		if err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
		jsonResponse(w, data)

	case "leak-smart":
		mode := r.URL.Query().Get("mode")
		if mode == "" {
			mode = "dense"
		}
		topN := queryInt(r, "top", 300)
		data, err := s.db.QueryLeakHistoTimeSeries(pid, mode, from, to, topN)
		if err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
		jsonResponse(w, data)

	case "leak-latest":
		mode := r.URL.Query().Get("mode")
		if mode == "" {
			mode = "dense"
		}
		data, err := s.db.QueryLeakLatest(pid, mode)
		if err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
		jsonResponse(w, data)

	case "leak-classes":
		mode := r.URL.Query().Get("mode")
		if mode == "" {
			mode = "dense"
		}
		classesParam := r.URL.Query().Get("classes")
		var classes []string
		if classesParam != "" {
			classes = strings.Split(classesParam, ",")
		}
		data, err := s.db.QueryLeakClasses(pid, mode, from, to, classes)
		if err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
		jsonResponse(w, data)

	case "hotcpu":
		threshold := queryFloat(r, "threshold", 10)
		data, err := analyzer.DetectCPUHotspots(s.db, pid, from, to, threshold)
		if err != nil {
			jsonError(w, err.Error(), 500)
			return
		}
		jsonResponse(w, data)

	default:
		jsonError(w, "unknown resource: "+resource, 404)
	}
}

// --- Page Handlers ---

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/dashboard", http.StatusFound)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data, err := staticFiles.ReadFile("static/dashboard.html")
	if err != nil {
		http.Error(w, "template not found", 500)
		return
	}
	w.Write(data)
}

func (s *Server) handleProcessPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	path := strings.TrimPrefix(r.URL.Path, "/process/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 2 {
		http.Error(w, "invalid path", 400)
		return
	}
	page := parts[1]

	var file string
	switch page {
	case "memory":
		file = "static/memory.html"
	case "cpu":
		file = "static/cpu.html"
	case "threads":
		file = "static/threads.html"
	case "heap":
		file = "static/heap.html"
	case "leak":
		file = "static/leak.html"
	case "leak-smart":
		file = "static/leak_smart.html"
	case "hotcpu":
		file = "static/hotcpu.html"
	default:
		http.NotFound(w, r)
		return
	}

	data, err := staticFiles.ReadFile(file)
	if err != nil {
		http.Error(w, "template not found", 500)
		return
	}
	w.Write(data)
}

// --- Helpers ---

func parseTimeRange(r *http.Request) (time.Time, time.Time) {
	fromStr := r.URL.Query().Get("from")
	toStr := r.URL.Query().Get("to")

	to := time.Now()
	from := to.Add(-1 * time.Hour) // default 1h

	if fromStr != "" {
		if d, err := time.ParseDuration(fromStr); err == nil {
			from = to.Add(-d)
		} else if t, err := time.Parse("2006-01-02T15:04:05", fromStr); err == nil {
			from = t
		}
	}
	if toStr != "" {
		if d, err := time.ParseDuration(toStr); err == nil {
			to = time.Now().Add(-d)
		} else if t, err := time.Parse("2006-01-02T15:04:05", toStr); err == nil {
			to = t
		}
	}
	return from, to
}

func queryInt(r *http.Request, key string, defaultVal int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return defaultVal
	}
	return n
}

func queryFloat(r *http.Request, key string, defaultVal float64) float64 {
	v := r.URL.Query().Get(key)
	if v == "" {
		return defaultVal
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return defaultVal
	}
	return n
}

func jsonResponse(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
