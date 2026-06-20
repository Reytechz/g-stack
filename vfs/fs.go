package vfs

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"g-stack/database"
	"g-stack/drive"

	"github.com/google/uuid"
	"golang.org/x/net/webdav"
)

const ChunkSize = 10 * 1024 * 1024 // 10 MB per chunk

type uploadTask struct {
	Name         string             `json:"name"`
	tempPath     string             `json:"-"`
	TotalSize    int64              `json:"total_size"`
	UploadedSize int64              `json:"uploaded_size"`
	cancel       context.CancelFunc `json:"-"`
}

type GStackFS struct {
	db           *database.DB
	driveManager *drive.DriveManager
	tempDir      string
	mu           sync.RWMutex
	uploading    map[string]uploadTask // nodeID -> uploadTask
}

func NewGStackFS(db *database.DB, dm *drive.DriveManager, tempDir string) (*GStackFS, error) {
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}
	return &GStackFS{
		db:           db,
		driveManager: dm,
		tempDir:      tempDir,
		uploading:    make(map[string]uploadTask),
	}, nil
}

// FileSystem implementation

func (fs *GStackFS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	parentPath, dirName := filepath.Split(filepath.Clean(name))
	parent, err := fs.db.ResolvePath(parentPath)
	if err != nil {
		return err
	}
	if parent == nil {
		return os.ErrNotExist
	}
	if !parent.IsDir {
		return fmt.Errorf("parent is not a directory")
	}

	// Check if already exists
	existing, err := fs.db.ResolvePath(name)
	if err != nil {
		return err
	}
	if existing != nil {
		return os.ErrExist
	}

	newNode := database.VirtualNode{
		ID:        uuid.New().String(),
		ParentID:  sql.NullString{String: parent.ID, Valid: true},
		Name:      dirName,
		IsDir:     true,
		Size:      0,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	return fs.db.CreateNode(newNode)
}

func (fs *GStackFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	cleanName := filepath.Clean(name)
	node, err := fs.db.ResolvePath(cleanName)
	if err != nil {
		log.Printf("[VFS Err] OpenFile ResolvePath failed for %s: %v", cleanName, err)
		return nil, err
	}

	isWrite := (flag&os.O_WRONLY != 0) || (flag&os.O_RDWR != 0) || (flag&os.O_CREATE != 0)

	if isWrite {
		parentPath, fileName := filepath.Split(cleanName)
		parent, err := fs.db.ResolvePath(parentPath)
		if err != nil {
			log.Printf("[VFS Err] OpenFile ResolvePath for parent %s failed: %v", parentPath, err)
			return nil, err
		}
		if parent == nil {
			log.Printf("[VFS Err] OpenFile: parent not found for path %s (parentPath: %s)", cleanName, parentPath)
			return nil, os.ErrNotExist
		}

		if node != nil {
			// If there's an active background upload for this node, cancel it.
			fs.mu.Lock()
			if task, ok := fs.uploading[node.ID]; ok {
				log.Printf("[VFS] OpenFile write: canceling active background upload for node ID %s", node.ID)
				task.cancel()
				delete(fs.uploading, node.ID)
				_ = os.Remove(task.tempPath)
			}
			fs.mu.Unlock()
		}

		if node == nil {
			// Create new virtual node
			node = &database.VirtualNode{
				ID:        uuid.New().String(),
				ParentID:  sql.NullString{String: parent.ID, Valid: true},
				Name:      fileName,
				IsDir:     false,
				Size:      0,
				CreatedAt: time.Now(),
				UpdatedAt: time.Now(),
			}
			if err := fs.db.CreateNode(*node); err != nil {
				log.Printf("[VFS Err] OpenFile CreateNode failed for %s: %v", fileName, err)
				return nil, err
			}
		} else if flag&os.O_TRUNC != 0 {
			// If truncating, delete existing chunks
			mappings, err := fs.db.GetFileMappings(node.ID)
			if err == nil {
				for _, m := range mappings {
					client, err := fs.driveManager.GetClient(m.GoogleAccountID)
					if err == nil {
						_ = client.DeleteFile(ctx, m.GoogleFileID)
					}
				}
			}
			// Delete database mappings
			_, _ = fs.db.Exec("DELETE FROM file_mappings WHERE node_id = ?", node.ID)
			_ = fs.db.UpdateNodeSize(node.ID, 0)
			node.Size = 0
		}

		tempPath := filepath.Join(fs.tempDir, uuid.New().String())
		tempFile, err := os.OpenFile(tempPath, os.O_CREATE|os.O_RDWR, 0666)
		if err != nil {
			log.Printf("[VFS Err] OpenFile failed to create local temp file at %s: %v", tempPath, err)
			return nil, fmt.Errorf("failed to create local temp file: %w", err)
		}

		return &virtualWritableFile{
			fs:       fs,
			node:     node,
			tempFile: tempFile,
			tempPath: tempPath,
			ctx:      ctx,
		}, nil
	}

	if node == nil {
		log.Printf("[VFS Err] OpenFile read failed: node not found for %s", cleanName)
		return nil, os.ErrNotExist
	}

	if node.IsDir {
		return &virtualDir{
			fs:   fs,
			node: *node,
			ctx:  ctx,
		}, nil
	}

	// Check if this file is currently uploading in the background
	fs.mu.RLock()
	task, isUploading := fs.uploading[node.ID]
	fs.mu.RUnlock()

	if isUploading {
		f, err := os.Open(task.tempPath)
		if err != nil {
			return nil, fmt.Errorf("failed to open local cached file: %w", err)
		}
		stat, err := f.Stat()
		if err == nil {
			node.Size = stat.Size()
		}
		return &localCachedFile{
			File: f,
			node: *node,
		}, nil
	}

	return &virtualFile{
		fs:     fs,
		node:   *node,
		offset: 0,
		ctx:    ctx,
	}, nil
}

func (fs *GStackFS) RemoveAll(ctx context.Context, name string) error {
	node, err := fs.db.ResolvePath(name)
	if err != nil {
		return err
	}
	if node == nil {
		return os.ErrNotExist
	}

	// Helper to delete recursively if directory
	var deleteNodeAndPhysicalFiles func(n database.VirtualNode) error
	deleteNodeAndPhysicalFiles = func(n database.VirtualNode) error {
		if n.IsDir {
			children, err := fs.db.ListChildren(n.ID)
			if err != nil {
				return err
			}
			for _, child := range children {
				if err := deleteNodeAndPhysicalFiles(child); err != nil {
					return err
				}
			}
		} else {
			// If there's an active background upload for this node, cancel it.
			fs.mu.Lock()
			if task, ok := fs.uploading[n.ID]; ok {
				task.cancel()
				delete(fs.uploading, n.ID)
				_ = os.Remove(task.tempPath)
			}
			fs.mu.Unlock()

			// Get physical file mappings
			mappings, err := fs.db.GetFileMappings(n.ID)
			if err == nil {
				for _, m := range mappings {
					client, err := fs.driveManager.GetClient(m.GoogleAccountID)
					if err == nil {
						_ = client.DeleteFile(ctx, m.GoogleFileID)
					}
				}
			}
		}
		return fs.db.DeleteNode(n.ID)
	}

	return deleteNodeAndPhysicalFiles(*node)
}

func (fs *GStackFS) Rename(ctx context.Context, oldName, newName string) error {
	node, err := fs.db.ResolvePath(oldName)
	if err != nil {
		return err
	}
	if node == nil {
		return os.ErrNotExist
	}

	newParentPath, newFileName := filepath.Split(filepath.Clean(newName))
	newParent, err := fs.db.ResolvePath(newParentPath)
	if err != nil {
		return err
	}
	if newParent == nil {
		return os.ErrNotExist
	}

	// If parent changes, move node
	if !node.ParentID.Valid || node.ParentID.String != newParent.ID {
		if err := fs.db.MoveNode(node.ID, newParent.ID); err != nil {
			return err
		}
	}

	// Rename node
	if node.Name != newFileName {
		if err := fs.db.RenameNode(node.ID, newFileName); err != nil {
			return err
		}
	}

	return nil
}

func (fs *GStackFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	node, err := fs.db.ResolvePath(name)
	if err != nil {
		return nil, err
	}
	if node == nil {
		return nil, os.ErrNotExist
	}

	// If this node is currently uploading in the background, report its local temp file size
	fs.mu.RLock()
	task, isUploading := fs.uploading[node.ID]
	fs.mu.RUnlock()
	if isUploading {
		stat, err := os.Stat(task.tempPath)
		if err == nil {
			node.Size = stat.Size()
		}
	}

	return VirtualFileInfo{node: *node}, nil
}

// FileInfo implementation for WebDAV

type VirtualFileInfo struct {
	node database.VirtualNode
}

func (vi VirtualFileInfo) Name() string       { return vi.node.Name }
func (vi VirtualFileInfo) Size() int64        { return vi.node.Size }
func (vi VirtualFileInfo) Mode() os.FileMode  {
	if vi.node.IsDir {
		return os.ModeDir | 0755
	}
	return 0644
}
func (vi VirtualFileInfo) ModTime() time.Time { return vi.node.UpdatedAt }
func (vi VirtualFileInfo) IsDir() bool        { return vi.node.IsDir }
func (vi VirtualFileInfo) Sys() interface{}   { return nil }

// virtualDir File implementation for directories

type virtualDir struct {
	fs          *GStackFS
	node        database.VirtualNode
	ctx         context.Context
	children    []database.VirtualNode
	readOffset  int
	initialized bool
}

func (d *virtualDir) Close() error               { return nil }
func (d *virtualDir) Read(p []byte) (int, error) { return 0, io.EOF }
func (d *virtualDir) Seek(offset int64, whence int) (int64, error) {
	if whence == 0 && offset == 0 {
		d.readOffset = 0
		return 0, nil
	}
	return 0, fmt.Errorf("seek on directory only supported for resetting offset (0, SeekStart)")
}
func (d *virtualDir) Write(p []byte) (int, error) {
	return 0, fmt.Errorf("cannot write to directory")
}
func (d *virtualDir) Stat() (os.FileInfo, error) {
	return VirtualFileInfo{node: d.node}, nil
}
func (d *virtualDir) Readdir(count int) ([]os.FileInfo, error) {
	if !d.initialized {
		children, err := d.fs.db.ListChildren(d.node.ID)
		if err != nil {
			return nil, err
		}
		// If any child is currently uploading in the background, report its local temp file size
		d.fs.mu.RLock()
		for i := range children {
			if task, ok := d.fs.uploading[children[i].ID]; ok {
				stat, err := os.Stat(task.tempPath)
				if err == nil {
					children[i].Size = stat.Size()
				}
			}
		}
		d.fs.mu.RUnlock()

		d.children = children
		d.readOffset = 0
		d.initialized = true
	}

	if count <= 0 {
		if d.readOffset >= len(d.children) {
			return nil, nil
		}
		rem := d.children[d.readOffset:]
		d.readOffset = len(d.children)
		infos := make([]os.FileInfo, len(rem))
		for i, child := range rem {
			infos[i] = VirtualFileInfo{node: child}
		}
		return infos, nil
	}

	if d.readOffset >= len(d.children) {
		return nil, io.EOF
	}

	limit := d.readOffset + count
	if limit > len(d.children) {
		limit = len(d.children)
	}

	chunk := d.children[d.readOffset:limit]
	d.readOffset = limit

	infos := make([]os.FileInfo, len(chunk))
	for i, child := range chunk {
		infos[i] = VirtualFileInfo{node: child}
	}

	return infos, nil
}

// virtualFile File implementation for reading files

type virtualFile struct {
	fs     *GStackFS
	node   database.VirtualNode
	offset int64
	ctx    context.Context
}

func (f *virtualFile) Close() error { return nil }

func (f *virtualFile) Read(p []byte) (int, error) {
	if f.offset >= f.node.Size {
		return 0, io.EOF
	}

	toRead := int64(len(p))
	if f.offset+toRead > f.node.Size {
		toRead = f.node.Size - f.offset
	}

	if toRead <= 0 {
		return 0, io.EOF
	}

	mappings, err := f.fs.db.GetFileMappings(f.node.ID)
	if err != nil {
		return 0, err
	}

	totalRead := 0
	var chunkStart int64 = 0
	for _, m := range mappings {
		chunkEnd := chunkStart + m.ChunkSize

		// Check if the current file offset falls within this chunk
		if f.offset < chunkEnd && f.offset+toRead > chunkStart {
			startInChunk := int64(0)
			if f.offset > chunkStart {
				startInChunk = f.offset - chunkStart
			}

			// Calculate end position in chunk
			bytesLeftToRead := toRead - int64(totalRead)
			endInChunk := m.ChunkSize - 1
			if startInChunk+bytesLeftToRead < m.ChunkSize {
				endInChunk = startInChunk + bytesLeftToRead - 1
			}

			length := endInChunk - startInChunk + 1

			client, err := f.fs.driveManager.GetClient(m.GoogleAccountID)
			if err != nil {
				return totalRead, fmt.Errorf("failed to find drive client for chunk: %w", err)
			}

			reader, err := client.DownloadChunkRange(f.ctx, m.GoogleFileID, startInChunk, endInChunk)
			if err != nil {
				return totalRead, fmt.Errorf("failed to stream chunk range: %w", err)
			}

			n, err := io.ReadFull(reader, p[totalRead:totalRead+int(length)])
			reader.Close()
			if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
				return totalRead, err
			}

			totalRead += n
			f.offset += int64(n)

			if int64(totalRead) >= toRead {
				break
			}
		}
		chunkStart = chunkEnd
	}

	if totalRead == 0 {
		return 0, io.EOF
	}

	return totalRead, nil
}

func (f *virtualFile) Seek(offset int64, whence int) (int64, error) {
	var newOffset int64
	switch whence {
	case io.SeekStart:
		newOffset = offset
	case io.SeekCurrent:
		newOffset = f.offset + offset
	case io.SeekEnd:
		newOffset = f.node.Size + offset
	default:
		return 0, fmt.Errorf("invalid whence")
	}

	if newOffset < 0 {
		return 0, fmt.Errorf("negative offset")
	}

	f.offset = newOffset
	return f.offset, nil
}

func (f *virtualFile) Write(p []byte) (int, error) {
	return 0, fmt.Errorf("read-only file")
}

func (f *virtualFile) Stat() (os.FileInfo, error) {
	return VirtualFileInfo{node: f.node}, nil
}

func (f *virtualFile) Readdir(count int) ([]os.FileInfo, error) {
	return nil, fmt.Errorf("not a directory")
}

// virtualWritableFile File implementation for writing files

type virtualWritableFile struct {
	fs       *GStackFS
	node     *database.VirtualNode
	tempFile *os.File
	tempPath string
	ctx      context.Context
	mu       sync.Mutex
	isClosed bool
}

func (w *virtualWritableFile) Read(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.tempFile.Read(p)
}

func (w *virtualWritableFile) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.tempFile.Write(p)
}

func (w *virtualWritableFile) Seek(offset int64, whence int) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.tempFile.Seek(offset, whence)
}

func (w *virtualWritableFile) Stat() (os.FileInfo, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	info, err := w.tempFile.Stat()
	if err != nil {
		return nil, err
	}
	return VirtualFileInfo{
		node: database.VirtualNode{
			ID:        w.node.ID,
			ParentID:  w.node.ParentID,
			Name:      w.node.Name,
			IsDir:     w.node.IsDir,
			Size:      info.Size(),
			CreatedAt: w.node.CreatedAt,
			UpdatedAt: time.Now(),
		},
	}, nil
}

func (w *virtualWritableFile) Readdir(count int) ([]os.FileInfo, error) {
	return nil, fmt.Errorf("not a directory")
}

func (w *virtualWritableFile) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.isClosed {
		return nil
	}
	w.isClosed = true

	// Sync and close local temp file so it is flushed to disk
	_ = w.tempFile.Sync()
	_ = w.tempFile.Close()

	// Get file size
	stat, err := os.Stat(w.tempPath)
	if err != nil {
		_ = os.Remove(w.tempPath)
		return fmt.Errorf("failed to stat temp file: %w", err)
	}
	totalSize := stat.Size()

	if totalSize == 0 {
		// Empty file. Just update node in DB and clean up.
		_ = os.Remove(w.tempPath)
		return w.fs.db.UpdateNodeSize(w.node.ID, 0)
	}

	// Start background upload
	bgCtx, cancel := context.WithCancel(context.Background())

	w.fs.mu.Lock()
	w.fs.uploading[w.node.ID] = uploadTask{
		Name:         w.node.Name,
		tempPath:     w.tempPath,
		TotalSize:    totalSize,
		UploadedSize: 0,
		cancel:       cancel,
	}
	w.fs.mu.Unlock()

	go w.fs.backgroundUpload(bgCtx, w.node, w.tempPath, totalSize)

	return nil
}

func (fs *GStackFS) backgroundUpload(ctx context.Context, node *database.VirtualNode, tempPath string, totalSize int64) {
	// Clean up on exit
	defer func() {
		fs.mu.Lock()
		delete(fs.uploading, node.ID)
		fs.mu.Unlock()
		_ = os.Remove(tempPath)
	}()

	// Open the temp file for reading
	file, err := os.Open(tempPath)
	if err != nil {
		log.Printf("[Upload] Failed to open temp file %s for background upload: %v", tempPath, err)
		return
	}
	defer file.Close()

	// Fetch all connected Google accounts to distribute chunks
	accounts, err := fs.db.GetAccounts()
	if err != nil {
		log.Printf("[Upload] Failed to list target accounts for background upload of %s: %v", node.Name, err)
		return
	}
	if len(accounts) == 0 {
		log.Printf("[Upload] Failed to background upload %s: no connected Google Drive accounts", node.Name)
		return
	}

	// Perform Chunked Upload
	var currentOffset int64 = 0
	chunkIndex := 0

	for currentOffset < totalSize {
		// Check for context cancellation
		select {
		case <-ctx.Done():
			log.Printf("[Upload] Background upload of %s was canceled/aborted", node.Name)
			return
		default:
		}

		chunkBytesToRead := int64(ChunkSize)
		if currentOffset+chunkBytesToRead > totalSize {
			chunkBytesToRead = totalSize - currentOffset
		}

		// Select account with the most free space (capacity - used_space)
		var bestAccount *database.Account
		var maxFreeSpace int64 = -1

		for i := range accounts {
			free := accounts[i].Capacity - accounts[i].UsedSpace
			if free > maxFreeSpace {
				maxFreeSpace = free
				bestAccount = &accounts[i]
			}
		}

		if bestAccount == nil || maxFreeSpace < chunkBytesToRead {
			log.Printf("[Upload] Failed to background upload %s: aggregate storage is full", node.Name)
			return
		}

		// Use io.NewSectionReader to provide a seekable reader with a known size!
		chunkReader := io.NewSectionReader(file, currentOffset, chunkBytesToRead)

		client, err := fs.driveManager.GetClient(bestAccount.ID)
		if err != nil {
			log.Printf("[Upload] Failed to get client for account %s during upload of %s: %v", bestAccount.ID, node.Name, err)
			return
		}

		chunkName := fmt.Sprintf("chunk_%s_%d", node.ID, chunkIndex)
		googleFileID, err := client.UploadChunk(ctx, chunkName, chunkReader)
		if err != nil {
			log.Printf("[Upload] Failed to upload chunk %d of %s to %s: %v", chunkIndex, node.Name, bestAccount.ID, err)
			return
		}

		// Store mapping in database
		mapping := database.FileMapping{
			ID:              uuid.New().String(),
			NodeID:          node.ID,
			ChunkIndex:      chunkIndex,
			GoogleAccountID: bestAccount.ID,
			GoogleFileID:    googleFileID,
			ChunkSize:       chunkBytesToRead,
		}

		if err := fs.db.AddFileMapping(mapping); err != nil {
			// Try to clean up from Google Drive on failure
			_ = client.DeleteFile(ctx, googleFileID)
			log.Printf("[Upload] Failed to save chunk metadata for %s: %v", node.Name, err)
			return
		}

		// Update database account usage
		bestAccount.UsedSpace += chunkBytesToRead
		_ = fs.db.SaveAccount(*bestAccount)

		currentOffset += chunkBytesToRead
		chunkIndex++

		// Update uploaded size for progress tracking
		fs.mu.Lock()
		if task, ok := fs.uploading[node.ID]; ok {
			task.UploadedSize = currentOffset
			fs.uploading[node.ID] = task
		}
		fs.mu.Unlock()
	}

	// Update node size
	if err := fs.db.UpdateNodeSize(node.ID, totalSize); err != nil {
		log.Printf("[Upload] Failed to update node size in DB for %s: %v", node.Name, err)
	} else {
		log.Printf("[Upload] Successfully uploaded %s (%d bytes) in background", node.Name, totalSize)
	}
}

// localCachedFile wraps *os.File to serve read requests from local cached file
type localCachedFile struct {
	*os.File
	node database.VirtualNode
}

func (l *localCachedFile) Stat() (os.FileInfo, error) {
	stat, err := l.File.Stat()
	if err == nil {
		l.node.Size = stat.Size()
	}
	return VirtualFileInfo{node: l.node}, nil
}

func (l *localCachedFile) Readdir(count int) ([]os.FileInfo, error) {
	return nil, fmt.Errorf("not a directory")
}


