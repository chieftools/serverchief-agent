package helper

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testActions() Actions {
	return Actions{
		ActionPing: {Run: func(context.Context, map[string]string) Result { return Result{Stdout: []byte("pong")} }},
		"echo": {
			Validate: func(args map[string]string) error {
				if len(args) != 1 || args["word"] == "" || strings.ContainsAny(args["word"], " ;/") {
					return errors.New("word must be one plain word")
				}
				return nil
			},
			Run: func(_ context.Context, args map[string]string) Result { return Result{Stdout: []byte(args["word"])} },
		},
	}
}

func serveDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "helper")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// serving runs Serve in a loop, as the path unit does whenever a request appears.
func serving(t *testing.T, dir string, actions Actions) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		for ctx.Err() == nil {
			if err := Serve(ctx, dir, actions); err != nil && ctx.Err() == nil {
				t.Errorf("Serve() error = %v", err)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	return func() { cancel(); wait.Wait() }
}

func TestClientRunsActionsThroughTheHelper(t *testing.T) {
	dir := serveDir(t)
	stop := serving(t, dir, testActions())
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := Client{Dir: dir}.Do(ctx, "echo", map[string]string{"word": "hello"})
	if err != nil || string(result.Stdout) != "hello" {
		t.Fatalf("Do() = %+v, %v", result, err)
	}
	if _, err := (Client{Dir: dir}).Do(ctx, "echo", map[string]string{"word": "rm -rf /"}); err == nil || !strings.Contains(err.Error(), "invalid arguments") {
		t.Fatalf("Do(invalid) error = %v", err)
	}
	if _, err := (Client{Dir: dir}).Do(ctx, "shell", nil); err == nil || !strings.Contains(err.Error(), "unknown helper action") {
		t.Fatalf("Do(unknown) error = %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left %d files behind", len(entries))
	}
}

func TestClientGivesUpWithoutAHelper(t *testing.T) {
	dir := serveDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := (Client{Dir: dir}).Do(ctx, ActionPing, nil); err == nil || !strings.Contains(err.Error(), "did not answer ping") {
		t.Fatalf("Do() error = %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left the request behind: %d files", len(entries))
	}
}

func TestServeIgnoresSymlinkedRequestsAndNeverWritesThroughAResultSymlink(t *testing.T) {
	dir := serveDir(t)
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 32)
	request, _ := json.Marshal(Request{ID: id, Action: ActionPing, CreatedAt: time.Now()})
	target := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(target, request, 0o600); err != nil {
		t.Fatal(err)
	}
	// A symlinked request is refused; a symlink planted at the result path is replaced, not followed
	if err := os.Symlink(target, filepath.Join(dir, id+requestSuffix)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, id+resultSuffix)); err != nil {
		t.Fatal(err)
	}

	if err := Serve(context.Background(), dir, testActions()); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	if data, _ := os.ReadFile(victim); string(data) != "untouched" {
		t.Fatalf("victim = %q", data)
	}
	data, err := os.ReadFile(filepath.Join(dir, id+resultSuffix))
	if err != nil {
		t.Fatal(err)
	}
	var result Result
	if err := json.Unmarshal(data, &result); err != nil || !strings.Contains(result.Error, "open the request") {
		t.Fatalf("result = %+v, %v", result, err)
	}
}

func TestServeRefusesADirectoryOthersCanWrite(t *testing.T) {
	dir := serveDir(t)
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := Serve(context.Background(), dir, testActions()); err == nil || !strings.Contains(err.Error(), "writable by others") {
		t.Fatalf("Serve() error = %v", err)
	}
}

func TestLocalRunsActionsInProcess(t *testing.T) {
	result, err := Local{Actions: testActions()}.Do(context.Background(), "echo", map[string]string{"word": "local"})
	if err != nil || string(result.Stdout) != "local" {
		t.Fatalf("Do() = %+v, %v", result, err)
	}
	if _, err := (Local{Actions: testActions()}).Do(context.Background(), ActionPing, map[string]string{"x": "y"}); err == nil {
		t.Fatal("Do() accepted arguments for an action without any")
	}
}
