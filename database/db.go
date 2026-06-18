package database

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/glebarez/go-sqlite"
)

type Account struct {
	ID           string
	RefreshToken string
	Capacity     int64
	UsedSpace    int64
}

type VirtualNode struct {
	ID        string
	ParentID  sql.NullString
	Name      string
	IsDir     bool
	Size      int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

type FileMapping struct {
	ID              string
	NodeID          string
	ChunkIndex      int
	GoogleAccountID string
	GoogleFileID    string
	ChunkSize       int64
}

type DB struct {
	*sql.DB
}

func InitDB(dbPath string) (*DB, error) {
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite db: %w", err)
	}

	db := &DB{conn}

	// Create tables
	if err := db.createTables(); err != nil {
		db.Close()
		return nil, err
	}

	// Ensure root node exists
	if err := db.ensureRootNode(); err != nil {
		db.Close()
		return nil, err
	}

	return db, nil
}

func (db *DB) createTables() error {
	schema := []string{
		`CREATE TABLE IF NOT EXISTS accounts (
			id TEXT PRIMARY KEY,
			refresh_token TEXT NOT NULL,
			capacity INTEGER NOT NULL,
			used_space INTEGER NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS virtual_nodes (
			id TEXT PRIMARY KEY,
			parent_id TEXT,
			name TEXT NOT NULL,
			is_dir BOOLEAN NOT NULL,
			size INTEGER DEFAULT 0,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY(parent_id) REFERENCES virtual_nodes(id) ON DELETE CASCADE,
			UNIQUE(parent_id, name)
		);`,
		`CREATE TABLE IF NOT EXISTS file_mappings (
			id TEXT PRIMARY KEY,
			node_id TEXT NOT NULL,
			chunk_index INTEGER NOT NULL,
			google_account_id TEXT NOT NULL,
			google_file_id TEXT NOT NULL,
			chunk_size INTEGER NOT NULL,
			FOREIGN KEY(node_id) REFERENCES virtual_nodes(id) ON DELETE CASCADE,
			FOREIGN KEY(google_account_id) REFERENCES accounts(id),
			UNIQUE(node_id, chunk_index)
		);`,
	}

	for _, q := range schema {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("failed to execute migration: %w", err)
		}
	}
	return nil
}

func (db *DB) ensureRootNode() error {
	var exists bool
	err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM virtual_nodes WHERE id = 'root')").Scan(&exists)
	if err != nil {
		return err
	}
	if !exists {
		_, err = db.Exec(
			"INSERT INTO virtual_nodes (id, parent_id, name, is_dir, size) VALUES ('root', NULL, '', 1, 0)",
		)
		if err != nil {
			return fmt.Errorf("failed to insert root node: %w", err)
		}
	}
	return nil
}

// Account operations

func (db *DB) SaveAccount(acc Account) error {
	_, err := db.Exec(
		`INSERT INTO accounts (id, refresh_token, capacity, used_space) 
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET 
		 refresh_token = excluded.refresh_token,
		 capacity = excluded.capacity,
		 used_space = excluded.used_space`,
		acc.ID, acc.RefreshToken, acc.Capacity, acc.UsedSpace,
	)
	return err
}

func (db *DB) GetAccounts() ([]Account, error) {
	rows, err := db.Query("SELECT id, refresh_token, capacity, used_space FROM accounts")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var accs []Account
	for rows.Next() {
		var acc Account
		if err := rows.Scan(&acc.ID, &acc.RefreshToken, &acc.Capacity, &acc.UsedSpace); err != nil {
			return nil, err
		}
		accs = append(accs, acc)
	}
	return accs, nil
}

func (db *DB) RemoveAccount(id string) error {
	_, err := db.Exec("DELETE FROM accounts WHERE id = ?", id)
	return err
}

// Virtual Node operations

func (db *DB) GetNode(id string) (*VirtualNode, error) {
	var node VirtualNode
	err := db.QueryRow(
		"SELECT id, parent_id, name, is_dir, size, created_at, updated_at FROM virtual_nodes WHERE id = ?",
		id,
	).Scan(&node.ID, &node.ParentID, &node.Name, &node.IsDir, &node.Size, &node.CreatedAt, &node.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &node, nil
}

func (db *DB) ResolvePath(path string) (*VirtualNode, error) {
	cleanPath := filepath.Clean(path)
	if cleanPath == "/" || cleanPath == "." || cleanPath == "" {
		return db.GetNode("root")
	}

	parts := strings.Split(strings.Trim(cleanPath, "/"), "/")
	currentNodeID := "root"
	var node VirtualNode

	for _, part := range parts {
		err := db.QueryRow(
			`SELECT id, parent_id, name, is_dir, size, created_at, updated_at 
			 FROM virtual_nodes 
			 WHERE parent_id = ? AND name = ?`,
			currentNodeID, part,
		).Scan(&node.ID, &node.ParentID, &node.Name, &node.IsDir, &node.Size, &node.CreatedAt, &node.UpdatedAt)
		if err == sql.ErrNoRows {
			return nil, nil // Not found
		}
		if err != nil {
			return nil, err
		}
		currentNodeID = node.ID
	}

	return &node, nil
}

func (db *DB) CreateNode(node VirtualNode) error {
	if !node.ParentID.Valid {
		return fmt.Errorf("parent ID must be valid")
	}
	_, err := db.Exec(
		`INSERT INTO virtual_nodes (id, parent_id, name, is_dir, size, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		node.ID, node.ParentID.String, node.Name, node.IsDir, node.Size, node.CreatedAt, node.UpdatedAt,
	)
	return err
}

func (db *DB) UpdateNodeSize(id string, size int64) error {
	_, err := db.Exec(
		"UPDATE virtual_nodes SET size = ?, updated_at = ? WHERE id = ?",
		size, time.Now(), id,
	)
	return err
}

func (db *DB) RenameNode(id, newName string) error {
	_, err := db.Exec(
		"UPDATE virtual_nodes SET name = ?, updated_at = ? WHERE id = ?",
		newName, time.Now(), id,
	)
	return err
}

func (db *DB) MoveNode(id, newParentID string) error {
	_, err := db.Exec(
		"UPDATE virtual_nodes SET parent_id = ?, updated_at = ? WHERE id = ?",
		newParentID, time.Now(), id,
	)
	return err
}

func (db *DB) DeleteNode(id string) error {
	// ON DELETE CASCADE takes care of child nodes and file mappings in sqlite
	_, err := db.Exec("DELETE FROM virtual_nodes WHERE id = ?", id)
	return err
}

func (db *DB) ListChildren(parentID string) ([]VirtualNode, error) {
	rows, err := db.Query(
		`SELECT id, parent_id, name, is_dir, size, created_at, updated_at 
		 FROM virtual_nodes 
		 WHERE parent_id = ? 
		 ORDER BY is_dir DESC, name ASC`,
		parentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var nodes []VirtualNode
	for rows.Next() {
		var node VirtualNode
		if err := rows.Scan(&node.ID, &node.ParentID, &node.Name, &node.IsDir, &node.Size, &node.CreatedAt, &node.UpdatedAt); err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	return nodes, nil
}

// File Mapping operations

func (db *DB) AddFileMapping(m FileMapping) error {
	_, err := db.Exec(
		`INSERT INTO file_mappings (id, node_id, chunk_index, google_account_id, google_file_id, chunk_size)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		m.ID, m.NodeID, m.ChunkIndex, m.GoogleAccountID, m.GoogleFileID, m.ChunkSize,
	)
	return err
}

func (db *DB) GetFileMappings(nodeID string) ([]FileMapping, error) {
	rows, err := db.Query(
		`SELECT id, node_id, chunk_index, google_account_id, google_file_id, chunk_size 
		 FROM file_mappings 
		 WHERE node_id = ? 
		 ORDER BY chunk_index ASC`,
		nodeID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var mappings []FileMapping
	for rows.Next() {
		var m FileMapping
		if err := rows.Scan(&m.ID, &m.NodeID, &m.ChunkIndex, &m.GoogleAccountID, &m.GoogleFileID, &m.ChunkSize); err != nil {
			return nil, err
		}
		mappings = append(mappings, m)
	}
	return mappings, nil
}
