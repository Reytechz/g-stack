package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"g-stack/database"
	"g-stack/drive"
	"g-stack/vfs"
)

type Config struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	Addr         string `json:"addr"`          // Default: "localhost:8080"
	Username     string `json:"username"`      // Default: "admin"
	Password     string `json:"password"`      // Default: "admin"
	DBPath       string `json:"db_path"`       // Default: "gstack.db"
	TempDir      string `json:"temp_dir"`      // Default: "./temp"
}

func loadConfig() (*Config, error) {
	// Defaults
	cfg := &Config{
		Addr:     "localhost:8080",
		Username: "admin",
		Password: "admin",
		DBPath:   "gstack.db",
		TempDir:  "./temp",
	}

	// Try reading config.json
	file, err := os.Open("config.json")
	if err == nil {
		defer file.Close()
		if err := json.NewDecoder(file).Decode(cfg); err != nil {
			return nil, fmt.Errorf("failed to parse config.json: %w", err)
		}
	}

	// Override with Env vars if present
	if envID := os.Getenv("GOOGLE_CLIENT_ID"); envID != "" {
		cfg.ClientID = envID
	}
	if envSecret := os.Getenv("GOOGLE_CLIENT_SECRET"); envSecret != "" {
		cfg.ClientSecret = envSecret
	}
	if envAddr := os.Getenv("GSTACK_ADDR"); envAddr != "" {
		cfg.Addr = envAddr
	}
	if envUser := os.Getenv("GSTACK_USER"); envUser != "" {
		cfg.Username = envUser
	}
	if envPass := os.Getenv("GSTACK_PASS"); envPass != "" {
		cfg.Password = envPass
	}
	if envDB := os.Getenv("GSTACK_DB_PATH"); envDB != "" {
		cfg.DBPath = envDB
	}
	if envTemp := os.Getenv("GSTACK_TEMP_DIR"); envTemp != "" {
		cfg.TempDir = envTemp
	}

	return cfg, nil
}

func printCredentialsGuide() {
	fmt.Println("=====================================================================")
	fmt.Println(" ERROR: Google Client ID or Client Secret is missing!")
	fmt.Println("=====================================================================")
	fmt.Println("G-Stack requires Google OAuth 2.0 credentials to operate.")
	fmt.Println("To obtain them:")
	fmt.Println(" 1. Go to Google Cloud Console: https://console.cloud.google.com/")
	fmt.Println(" 2. Create a new project.")
	fmt.Println(" 3. Enable the 'Google Drive API' for your project.")
	fmt.Println(" 4. Go to 'APIs & Services' -> 'OAuth consent screen', configure it (External/Internal).")
	fmt.Println(" 5. Add '.../auth/drive.file' scope to your consent screen configuration.")
	fmt.Println(" 6. Go to 'Credentials' -> 'Create Credentials' -> 'OAuth client ID'.")
	fmt.Println(" 7. Select Application Type: 'Web application'.")
	fmt.Println(" 8. Set Authorized redirect URIs: http://localhost:8080/auth/callback")
	fmt.Println(" 9. Download the Client ID and Client Secret.")
	fmt.Println()
	fmt.Println("Configure G-Stack by either:")
	fmt.Println(" A) Exporting environment variables:")
	fmt.Println("    export GOOGLE_CLIENT_ID=\"your-client-id\"")
	fmt.Println("    export GOOGLE_CLIENT_SECRET=\"your-client-secret\"")
	fmt.Println()
	fmt.Println(" B) Creating a 'config.json' file in this directory:")
	fmt.Println("    {")
	fmt.Println("        \"client_id\": \"your-client-id\",")
	fmt.Println("        \"client_secret\": \"your-client-secret\",")
	fmt.Println("        \"addr\": \"localhost:8080\",")
	fmt.Println("        \"username\": \"admin\",")
	fmt.Println("        \"password\": \"admin\"")
	fmt.Println("    }")
	fmt.Println("=====================================================================")
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("Error loading config: %v", err)
	}

	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		printCredentialsGuide()
		os.Exit(1)
	}

	// Initialize Database
	db, err := database.InitDB(cfg.DBPath)
	if err != nil {
		log.Fatalf("Error initializing database: %v", err)
	}
	defer db.Close()

	// Setup redirect URL
	redirectURL := fmt.Sprintf("http://%s/auth/callback", cfg.Addr)
	
	// Initialize Drive Manager
	dm := drive.NewDriveManager(cfg.ClientID, cfg.ClientSecret, redirectURL)

	// Restore already connected accounts
	accounts, err := db.GetAccounts()
	if err != nil {
		log.Fatalf("Error querying saved accounts: %v", err)
	}

	ctx := context.Background()
	log.Printf("Starting G-Stack. Restoring %d Google accounts...", len(accounts))
	for _, acc := range accounts {
		log.Printf("Connecting Google Drive: %s", acc.ID)
		_, err := dm.AddAccount(ctx, acc.ID, acc.RefreshToken)
		if err != nil {
			log.Printf("Warning: Failed to reconnect account %s: %v", acc.ID, err)
		}
	}

	// Initialize Filesystem
	fs, err := vfs.NewGStackFS(db, dm, cfg.TempDir)
	if err != nil {
		log.Fatalf("Error setting up filesystem: %v", err)
	}

	// Start server
	server := vfs.NewWebDAVServer(db, dm, fs, cfg.Username, cfg.Password)
	if err := server.Start(cfg.Addr); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
