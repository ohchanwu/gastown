// Package health provides reusable health check functions for the Gas Town data plane.
// These checks are shared between the Doctor Dog (daemon/doctor_dog.go) and the
// gt health CLI command (cmd/health.go).
package health

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/util"
)

// TCPCheck performs a TCP connection check to host:port.
// Returns true if connection succeeds within the timeout.
func TCPCheck(host string, port int, timeout time.Duration) bool {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// LatencyCheck runs SELECT 1 against the Dolt server and returns the round-trip latency.
func LatencyCheck(host string, port int, timeout time.Duration) (time.Duration, error) {
	dsn := fmt.Sprintf("root@tcp(%s:%d)/?timeout=5s&readTimeout=10s", host, port)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return 0, fmt.Errorf("open connection: %w", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	start := time.Now()
	var result int
	if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&result); err != nil {
		return 0, fmt.Errorf("SELECT 1: %w", err)
	}
	return time.Since(start), nil
}

// DatabaseCount runs SHOW DATABASES and returns the count (excluding system databases).
func DatabaseCount(host string, port int) (int, []string, error) {
	dsn := fmt.Sprintf("root@tcp(%s:%d)/?timeout=5s&readTimeout=10s", host, port)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return 0, nil, err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rows, err := db.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()

	var databases []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		if name == "information_schema" || name == "mysql" {
			continue
		}
		databases = append(databases, name)
	}

	return len(databases), databases, nil
}

// LocalDoltServerResult summarizes actionable and report-only listeners.
type LocalDoltServerResult struct {
	ActionableCount int
	ActionablePIDs  []int
	UnknownCount    int
	UnknownPIDs     []int
}

// SummarizeLocalDoltServers keeps unknown listeners visible without making them actionable.
func SummarizeLocalDoltServers(inventory []doltserver.LocalDoltServer) LocalDoltServerResult {
	result := LocalDoltServerResult{}
	for _, server := range inventory {
		switch {
		case server.Actionable():
			result.ActionablePIDs = append(result.ActionablePIDs, server.PID)
		case server.Class == doltserver.DoltServerUnknown:
			result.UnknownPIDs = append(result.UnknownPIDs, server.PID)
		}
	}
	result.ActionableCount = len(result.ActionablePIDs)
	result.UnknownCount = len(result.UnknownPIDs)
	return result
}

// FindLocalDoltServers uses the shared classified listener inventory.
func FindLocalDoltServers(townRoot string) LocalDoltServerResult {
	return SummarizeLocalDoltServers(doltserver.InventoryLocalDoltServers(townRoot))
}

// BackupFreshness checks the age of the newest file in a directory.
// Returns zero time if the directory doesn't exist or is empty.
func BackupFreshness(dir string) time.Time {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return time.Time{}
	}

	var newest time.Time
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return newest
}

// JSONLGitFreshness returns the timestamp of the latest git commit in the JSONL archive.
func JSONLGitFreshness(gitRepo string) (time.Time, error) {
	if _, err := os.Stat(filepath.Join(gitRepo, ".git")); os.IsNotExist(err) {
		return time.Time{}, fmt.Errorf("not a git repo: %s", gitRepo)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "-C", gitRepo, "log", "-1", "--format=%ci")
	util.SetDetachedProcessGroup(cmd)
	output, err := cmd.Output()
	if err != nil {
		return time.Time{}, err
	}

	commitTimeStr := strings.TrimSpace(string(output))
	if commitTimeStr == "" {
		return time.Time{}, fmt.Errorf("no commits")
	}

	return time.Parse("2006-01-02 15:04:05 -0700", commitTimeStr)
}

// DirSize calculates the total size of files in a directory recursively.
func DirSize(path string) (int64, error) {
	var size int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	return size, err
}
