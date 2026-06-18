package vfs

import (
	"context"
	"database/sql"
	"fmt"
	"io"
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

type GStackFS struct {
	db           *database.DB
	driveManager *drive.DriveManager
	tempDir      string
}

func NewGStackFS(db *database.DB, dm *drive.DriveManager, tempDir string) (*GStackFS, error) {
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}
	return &GStackFS{
		db:           db,
		driveManager: dm,
		tempDir:      tempDir,
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
		return nil, err
	}

	isWrite := (flag&os.O_WRONLY != 0) || (flag&os.O_RDWR != 0) || (flag&os.O_CREATE != 0)

	if isWrite {
		parentPath, fileName := filepath.Split(cleanName)
		parent, err := fs.db.ResolvePath(parentPath)
		if err != nil {
			return nil, err
		}
		if parent == nil {
			return nil, os.ErrNotExist
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
		return nil, os.ErrNotExist
	}

	if node.IsDir {
		return &virtualDir{
			fs:   fs,
			node: *node,
			ctx:  ctx,
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

	// Sync local temp file
	_ = w.tempFile.Sync()
	_, _ = w.tempFile.Seek(0, io.SeekStart)

	// Clean up local temp file on return
	defer func() {
		w.tempFile.Close()
		_ = os.Remove(w.tempPath)
	}()

	// Get file size
	stat, err := w.tempFile.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat temp file: %w", err)
	}
	totalSize := stat.Size()

	if totalSize == 0 {
		// Empty file. Just update node in DB.
		return w.fs.db.UpdateNodeSize(w.node.ID, 0)
	}

	// Fetch all connected Google accounts to distribute chunks
	accounts, err := w.fs.db.GetAccounts()
	if err != nil {
		return fmt.Errorf("failed to list target accounts: %w", err)
	}
	if len(accounts) == 0 {
		return fmt.Errorf("cannot upload file: no connected Google Drive accounts")
	}

	// Perform Chunked Upload
	var currentOffset int64 = 0
	chunkIndex := 0

	for currentOffset < totalSize {
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
			return fmt.Errorf("out of space: aggregate storage is full")
		}

		// Read the chunk bytes
		chunkReader := io.LimitReader(w.tempFile, chunkBytesToRead)

		client, err := w.fs.driveManager.GetClient(bestAccount.ID)
		if err != nil {
			return fmt.Errorf("failed to get client for account %s: %w", bestAccount.ID, err)
		}

		chunkName := fmt.Sprintf("chunk_%s_%d", w.node.ID, chunkIndex)
		googleFileID, err := client.UploadChunk(w.ctx, chunkName, chunkReader)
		if err != nil {
			return fmt.Errorf("failed to upload chunk %d to %s: %w", chunkIndex, bestAccount.ID, err)
		}

		// Store mapping in database
		mapping := database.FileMapping{
			ID:              uuid.New().String(),
			NodeID:          w.node.ID,
			ChunkIndex:      chunkIndex,
			GoogleAccountID: bestAccount.ID,
			GoogleFileID:    googleFileID,
			ChunkSize:       chunkBytesToRead,
		}

		if err := w.fs.db.AddFileMapping(mapping); err != nil {
			// Try to clean up from Google Drive on failure
			_ = client.DeleteFile(w.ctx, googleFileID)
			return fmt.Errorf("failed to save chunk metadata: %w", err)
		}

		// Update database account usage
		bestAccount.UsedSpace += chunkBytesToRead
		_ = w.fs.db.SaveAccount(*bestAccount)

		currentOffset += chunkBytesToRead
		chunkIndex++
	}

	// Update node size
	return w.fs.db.UpdateNodeSize(w.node.ID, totalSize)
}

