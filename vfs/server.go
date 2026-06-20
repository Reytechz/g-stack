package vfs

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"

	"g-stack/database"
	"g-stack/drive"

	"golang.org/x/net/webdav"
)

type WebDAVServer struct {
	db           *database.DB
	driveManager *drive.DriveManager
	fs           *GStackFS
	handler      *webdav.Handler
	username     string
	password     string
}

func NewWebDAVServer(db *database.DB, dm *drive.DriveManager, fs *GStackFS, username, password string) *WebDAVServer {
	return &WebDAVServer{
		db:           db,
		driveManager: dm,
		fs:           fs,
		handler: &webdav.Handler{
			Prefix:     "/G-Stack",
			FileSystem: fs,
			LockSystem: webdav.NewMemLS(),
		},
		username: username,
		password: password,
	}
}

func (s *WebDAVServer) Start(addr string) error {
	mux := http.NewServeMux()

	// OAuth2 auth routes
	mux.HandleFunc("/auth/login", s.handleOAuthLogin)
	mux.HandleFunc("/auth/callback", s.handleOAuthCallback)

	// Status dashboard API
	mux.HandleFunc("/status", s.handleStatus)

	// Background uploads progress API
	mux.HandleFunc("/uploads", s.handleUploads)

	// Unlink account API
	mux.HandleFunc("/accounts/delete", s.handleAccountDelete)

	// WebDAV mount point
	mux.HandleFunc("/G-Stack", s.handleWebDAV)
	mux.HandleFunc("/G-Stack/", s.handleWebDAV)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/G-Stack/", http.StatusTemporaryRedirect)
	})

	fmt.Printf("G-Stack Server is running at http://%s\n", addr)
	fmt.Printf("Open http://%s/auth/login in your browser to add Google Drive accounts.\n", addr)
	fmt.Printf("Mount WebDAV endpoint at http://%s/G-Stack/ with Username: %s\n", addr, s.username)

	return http.ListenAndServe(addr, mux)
}

func (s *WebDAVServer) handleWebDAV(w http.ResponseWriter, r *http.Request) {
	log.Printf("WebDAV: %s %s", r.Method, r.URL.Path)
	// Authenticate WebDAV mounts (Basic Auth)
	if s.username != "" && s.password != "" {
		u, p, ok := r.BasicAuth()
		if !ok || u != s.username || p != s.password {
			// Consume request body to prevent "connection reset" / "broken pipe" errors 
			// in clients like KDE Dolphin (KIO) when uploading files larger than the TCP socket buffer.
			if r.Body != nil {
				_, _ = io.Copy(io.Discard, r.Body)
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="G-Stack WebDAV"`)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("Unauthorized"))
			return
		}
	}

	// Route to WebDAV Handler
	s.handler.ServeHTTP(w, r)
}

func (s *WebDAVServer) handleOAuthLogin(w http.ResponseWriter, r *http.Request) {
	// Generate Auth URL with force consent to ensure refresh token is returned
	authURL := s.driveManager.GetAuthURL()
	http.Redirect(w, r, authURL, http.StatusTemporaryRedirect)
}

func (s *WebDAVServer) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	code := r.FormValue("code")
	if code == "" {
		http.Error(w, "Missing authorization code", http.StatusBadRequest)
		return
	}

	// Exchange code for token
	token, err := s.driveManager.ExchangeCode(ctx, code)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to exchange token: %v", err), http.StatusInternalServerError)
		return
	}

	if token.RefreshToken == "" {
		http.Error(w, "Warning: No refresh token returned. Try removing app access in Google Security settings and logging in again.", http.StatusBadRequest)
		return
	}

	// Fetch user's email
	email, err := s.driveManager.FetchEmail(ctx, token.AccessToken)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to fetch user email: %v", err), http.StatusInternalServerError)
		return
	}

	// Add/initialize account in manager
	client, err := s.driveManager.AddAccount(ctx, email, token.RefreshToken)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to initialize drive: %v", err), http.StatusInternalServerError)
		return
	}

	// Fetch capacity quota
	capacity, used, err := client.GetQuota(ctx)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to fetch account quota: %v", err), http.StatusInternalServerError)
		return
	}

	// Save to SQLite
	acc := database.Account{
		ID:           email,
		RefreshToken: token.RefreshToken,
		Capacity:     capacity,
		UsedSpace:    used,
	}
	if err := s.db.SaveAccount(acc); err != nil {
		http.Error(w, fmt.Sprintf("Failed to save account to database: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(fmt.Sprintf(`
		<html>
		<body style="font-family: sans-serif; text-align: center; padding: 50px;">
			<h2 style="color: #2e7d32;">Success!</h2>
			<p>Connected Google Drive account: <strong>%s</strong></p>
			<p>Storage Capacity: %d GB</p>
			<p>You can close this tab and return to G-Stack.</p>
		</body>
		</html>
	`, email, capacity/(1024*1024*1024))))
}

type AccountStatus struct {
	Email     string `json:"email"`
	Capacity  int64  `json:"capacity"`
	UsedSpace int64  `json:"used_space"`
}

type StatusResponse struct {
	TotalCapacity  int64           `json:"total_capacity"`
	TotalUsedSpace int64           `json:"total_used_space"`
	FreeSpace      int64           `json:"free_space"`
	AccountsCount  int             `json:"accounts_count"`
	Accounts       []AccountStatus `json:"accounts"`
}

func (s *WebDAVServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	accounts, err := s.db.GetAccounts()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var totalCapacity int64
	var totalUsed int64
	statusAccs := make([]AccountStatus, len(accounts))

	for i, acc := range accounts {
		totalCapacity += acc.Capacity
		totalUsed += acc.UsedSpace
		statusAccs[i] = AccountStatus{
			Email:     acc.ID,
			Capacity:  acc.Capacity,
			UsedSpace: acc.UsedSpace,
		}
	}

	resp := StatusResponse{
		TotalCapacity:  totalCapacity,
		TotalUsedSpace: totalUsed,
		FreeSpace:      totalCapacity - totalUsed,
		AccountsCount:  len(accounts),
		Accounts:       statusAccs,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *WebDAVServer) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	email := r.URL.Query().Get("email")
	if email == "" {
		http.Error(w, "Missing email parameter", http.StatusBadRequest)
		return
	}

	if err := s.db.RemoveAccount(email); err != nil {
		http.Error(w, fmt.Sprintf("Failed to remove account from DB: %v", err), http.StatusInternalServerError)
		return
	}

	s.driveManager.RemoveClient(email)

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("Account unlinked successfully"))
}

func (s *WebDAVServer) handleUploads(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")

	s.fs.mu.RLock()
	activeUploads := make([]uploadTask, 0, len(s.fs.uploading))
	for _, task := range s.fs.uploading {
		activeUploads = append(activeUploads, task)
	}
	s.fs.mu.RUnlock()

	json.NewEncoder(w).Encode(activeUploads)
}

