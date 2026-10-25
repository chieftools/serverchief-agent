package helper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	requestSuffix = ".request.json"
	resultSuffix  = ".result.json"
	pollInterval  = 200 * time.Millisecond
	// staleAfter drops requests nobody answered, such as after a crash.
	staleAfter = time.Hour
)

// Client is the daemon's side: it writes a request and waits for the root
// helper's result.
type Client struct {
	Dir string
}

// Do writes the request and waits for its result until ctx ends.
func (client Client) Do(ctx context.Context, action string, args map[string]string) (Result, error) {
	request := Request{ID: newID(), Action: action, Args: args, CreatedAt: time.Now().UTC()}
	data, err := json.Marshal(request)
	if err != nil {
		return Result{}, err
	}
	requestPath := filepath.Join(client.Dir, request.ID+requestSuffix)
	resultPath := filepath.Join(client.Dir, request.ID+resultSuffix)
	if err := writeAtomic(client.Dir, requestPath, data); err != nil {
		return Result{}, fmt.Errorf("write the helper request: %w", err)
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		if data, err := os.ReadFile(resultPath); err == nil {
			_ = os.Remove(resultPath)
			var result Result
			if err := json.Unmarshal(data, &result); err != nil {
				return Result{}, fmt.Errorf("read the helper result: %w", err)
			}
			if result.Error != "" {
				return result, errors.New(result.Error)
			}
			return result, nil
		}
		select {
		case <-ctx.Done():
			_ = os.Remove(requestPath)
			return Result{}, fmt.Errorf("the root helper did not answer %s: %w", action, ctx.Err())
		case <-ticker.C:
		}
	}
}

// Serve runs every pending request in dir as root and returns once none is
// left. Request files must be regular files of the owner of dir, results
// are created next to them without following or replacing anything.
func Serve(ctx context.Context, dir string, actions Actions) error {
	owner, err := directoryOwner(dir)
	if err != nil {
		return err
	}
	removeStale(dir)
	for {
		names, err := pendingRequests(dir)
		if err != nil {
			return err
		}
		if len(names) == 0 {
			return nil
		}
		for _, name := range names {
			if err := ctx.Err(); err != nil {
				return err
			}
			serveOne(ctx, dir, name, owner, actions)
		}
	}
}

func serveOne(ctx context.Context, dir, name string, owner int, actions Actions) {
	requestPath := filepath.Join(dir, name)
	defer os.Remove(requestPath)

	id := strings.TrimSuffix(name, requestSuffix)
	if !idPattern.MatchString(id) {
		return
	}
	request, created, err := readRequest(requestPath, owner)
	if err != nil {
		writeResult(dir, id, owner, Result{ID: id, Error: err.Error()})
		return
	}
	if time.Since(created) > staleAfter || request.ID != id {
		return
	}
	writeResult(dir, id, owner, actions.run(ctx, request))
}

func pendingRequests(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), requestSuffix) {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}

// readRequest opens the request without following symlinks and only
// accepts a small regular file of the directory's owner.
func readRequest(path string, owner int) (Request, time.Time, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Request{}, time.Time{}, fmt.Errorf("open the request: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Request{}, time.Time{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxRequestBytes {
		return Request{}, time.Time{}, errors.New("the request is not a small regular file")
	}
	if uid, ok := fileOwner(info); !ok || uid != owner {
		return Request{}, time.Time{}, errors.New("the request belongs to another user")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxRequestBytes+1))
	if err != nil {
		return Request{}, time.Time{}, err
	}
	var request Request
	if err := json.Unmarshal(data, &request); err != nil {
		return Request{}, time.Time{}, fmt.Errorf("parse the request: %w", err)
	}
	return request, info.ModTime(), nil
}

// writeResult creates a new file (never following or reusing one), hands
// it to the directory's owner and renames it into place. A rename replaces
// a symlink at the destination instead of writing through it.
func writeResult(dir, id string, owner int, result Result) {
	data, err := json.Marshal(result)
	if err != nil {
		return
	}
	temporary := filepath.Join(dir, "."+id+".result.tmp")
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return
	}
	_, writeErr := file.Write(data)
	chownErr := file.Chown(owner, -1)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil || (chownErr != nil && os.Geteuid() == 0) {
		_ = os.Remove(temporary)
		return
	}
	if err := os.Rename(temporary, filepath.Join(dir, id+resultSuffix)); err != nil {
		_ = os.Remove(temporary)
	}
}

func writeAtomic(dir, path string, data []byte) error {
	file, err := os.CreateTemp(dir, ".request-*")
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		os.Remove(file.Name())
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(file.Name())
		return err
	}
	return os.Rename(file.Name(), path)
}

// directoryOwner refuses a directory anyone but its owner can write to,
// and returns that owner.
func directoryOwner(dir string) (int, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return 0, &fs.PathError{Op: "serve", Path: dir, Err: errors.New("not a directory")}
	}
	if info.Mode().Perm()&0o022 != 0 {
		return 0, &fs.PathError{Op: "serve", Path: dir, Err: errors.New("writable by others")}
	}
	uid, ok := fileOwner(info)
	if !ok {
		return 0, &fs.PathError{Op: "serve", Path: dir, Err: errors.New("unknown owner")}
	}
	return uid, nil
}

func fileOwner(info fs.FileInfo) (int, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(stat.Uid), true
}

// removeStale drops results a daemon gave up waiting for, and leftovers of
// interrupted writes.
func removeStale(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, resultSuffix) && !strings.HasSuffix(name, ".result.tmp") && !strings.HasPrefix(name, ".request-") {
			continue
		}
		if info, err := entry.Info(); err == nil && time.Since(info.ModTime()) > staleAfter {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}
