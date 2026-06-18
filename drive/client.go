package drive

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

type DriveClient struct {
	Email       string
	TokenSource oauth2.TokenSource
	Service     *drive.Service
	FolderID    string // The ID of the "G-Stack-Data" directory on this account
}

type DriveManager struct {
	oauthConfig *oauth2.Config
	clients     map[string]*DriveClient
	mu          sync.RWMutex
}

func NewDriveManager(clientID, clientSecret, redirectURL string) *DriveManager {
	config := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Endpoint:     google.Endpoint,
		Scopes:       []string{drive.DriveFileScope, drive.DriveMetadataReadonlyScope, "email"},
	}
	return &DriveManager{
		oauthConfig: config,
		clients:     make(map[string]*DriveClient),
	}
}

func (dm *DriveManager) GetAuthURL() string {
	// Request offline access and force approval to ensure refresh token is returned
	return dm.oauthConfig.AuthCodeURL("state-token", oauth2.AccessTypeOffline, oauth2.ApprovalForce)
}

func (dm *DriveManager) ExchangeCode(ctx context.Context, code string) (*oauth2.Token, error) {
	return dm.oauthConfig.Exchange(ctx, code)
}

func (dm *DriveManager) FetchEmail(ctx context.Context, accessToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://www.googleapis.com/oauth2/v2/userinfo", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to get user info, status: %d", resp.StatusCode)
	}

	var result struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.Email, nil
}

func (dm *DriveManager) AddAccount(ctx context.Context, email, refreshToken string) (*DriveClient, error) {
	dm.mu.Lock()
	defer dm.mu.Unlock()

	// Check if already loaded
	if client, ok := dm.clients[email]; ok {
		return client, nil
	}

	token := &oauth2.Token{
		RefreshToken: refreshToken,
	}

	tokenSource := dm.oauthConfig.TokenSource(ctx, token)
	httpClient := oauth2.NewClient(ctx, tokenSource)

	service, err := drive.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("failed to create drive service for %s: %w", email, err)
	}

	client := &DriveClient{
		Email:       email,
		TokenSource: tokenSource,
		Service:     service,
	}

	// Ensure the "G-Stack-Data" folder exists
	folderID, err := client.ensureDataFolder(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to verify data folder for %s: %w", email, err)
	}
	client.FolderID = folderID

	dm.clients[email] = client
	return client, nil
}

func (dm *DriveManager) GetClient(email string) (*DriveClient, error) {
	dm.mu.RLock()
	client, ok := dm.clients[email]
	dm.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("account %s not loaded in drive manager", email)
	}
	return client, nil
}

func (dm *DriveManager) GetClients() []*DriveClient {
	dm.mu.RLock()
	defer dm.mu.RUnlock()

	clients := make([]*DriveClient, 0, len(dm.clients))
	for _, client := range dm.clients {
		clients = append(clients, client)
	}
	return clients
}

func (dm *DriveManager) RemoveClient(email string) {
	dm.mu.Lock()
	delete(dm.clients, email)
	dm.mu.Unlock()
}

// Find G-Stack-Data folder, or create it if missing
func (c *DriveClient) ensureDataFolder(ctx context.Context) (string, error) {
	query := "name = 'G-Stack-Data' and mimeType = 'application/vnd.google-apps.folder' and trashed = false"
	list, err := c.Service.Files.List().Q(query).Fields("files(id)").Context(ctx).Do()
	if err != nil {
		return "", err
	}

	if len(list.Files) > 0 {
		return list.Files[0].Id, nil
	}

	// Create new folder
	folder := &drive.File{
		Name:     "G-Stack-Data",
		MimeType: "application/vnd.google-apps.folder",
	}
	newFolder, err := c.Service.Files.Create(folder).Fields("id").Context(ctx).Do()
	if err != nil {
		return "", err
	}

	return newFolder.Id, nil
}

// Get Quota (Capacity and Used Space) for the account
func (c *DriveClient) GetQuota(ctx context.Context) (capacity int64, used int64, err error) {
	about, err := c.Service.About.Get().Fields("storageQuota").Context(ctx).Do()
	if err != nil {
		return 0, 0, err
	}
	return about.StorageQuota.Limit, about.StorageQuota.Usage, nil
}

// Upload a chunk/file to Google Drive inside the G-Stack-Data folder
func (c *DriveClient) UploadChunk(ctx context.Context, name string, reader io.Reader) (string, error) {
	f := &drive.File{
		Name:    name,
		Parents: []string{c.FolderID},
	}

	res, err := c.Service.Files.Create(f).Media(reader).Context(ctx).Do()
	if err != nil {
		return "", err
	}
	return res.Id, nil
}

// Download a specific range of bytes for a chunk
func (c *DriveClient) DownloadChunkRange(ctx context.Context, fileID string, start, end int64) (io.ReadCloser, error) {
	token, err := c.TokenSource.Token()
	if err != nil {
		return nil, fmt.Errorf("failed to get access token: %w", err)
	}

	url := fmt.Sprintf("https://www.googleapis.com/drive/v3/files/%s?alt=media", fileID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	
	// Add Range header if requested
	if start >= 0 && end >= 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	} else if start >= 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		resp.Body.Close()
		return nil, fmt.Errorf("bad status code on media download: %d", resp.StatusCode)
	}

	return resp.Body, nil
}

// Delete a file/chunk from Google Drive
func (c *DriveClient) DeleteFile(ctx context.Context, fileID string) error {
	return c.Service.Files.Delete(fileID).Context(ctx).Do()
}
