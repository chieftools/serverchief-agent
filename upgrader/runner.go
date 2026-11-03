package upgrader

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Output is a finished command: its combined stdout and stderr, in order.
type Output struct {
	Text     []byte
	ExitCode int
}

// Runner runs argv with extra environment variables, without a shell. A
// non-zero exit is not an error; failing to start is.
type Runner func(ctx context.Context, env []string, argv []string) (Output, error)

// lockedBuffer interleaves stdout and stderr as the program writes them.
type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *lockedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(data)
}

// ExecRunner runs commands with os/exec in the C locale, so the output
// the helper parses does not depend on the host's language.
func ExecRunner(ctx context.Context, env []string, argv []string) (Output, error) {
	if len(argv) == 0 {
		return Output{}, errors.New("no command")
	}
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Env = append(append(os.Environ(), "LC_ALL=C", "LANG=C"), env...)
	command.Dir = "/"
	command.WaitDelay = 10 * time.Second
	var combined lockedBuffer
	command.Stdout, command.Stderr = &combined, &combined
	err := command.Run()
	output := Output{Text: combined.buffer.Bytes()}
	var exit *exec.ExitError
	if errors.As(err, &exit) && ctx.Err() == nil {
		output.ExitCode = exit.ExitCode()
		return output, nil
	}
	if err != nil {
		output.ExitCode = -1
	}
	return output, err
}
