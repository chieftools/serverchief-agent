package upgrader

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Output is a finished command: its combined stdout and stderr, in order.
type Output struct {
	Text     []byte
	ExitCode int
	// Streamed is set when Text also went to the runner's live writer as
	// the program wrote it.
	Streamed bool
}

// Runner runs argv with extra environment variables, without a shell. A
// non-zero exit is not an error; failing to start is. A runner that can
// writes the output to live as it arrives and sets Output.Streamed; live
// may be nil.
type Runner func(ctx context.Context, env []string, argv []string, live io.Writer) (Output, error)

// lockedBuffer interleaves stdout and stderr as the program writes them,
// passing them on to live as they arrive.
type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	live   io.Writer
}

func (buffer *lockedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	if buffer.live != nil {
		_, _ = buffer.live.Write(data)
	}
	return buffer.buffer.Write(data)
}

// ExecRunner runs commands with os/exec in the C locale, so the output
// the helper parses does not depend on the host's language.
func ExecRunner(ctx context.Context, env []string, argv []string, live io.Writer) (Output, error) {
	if len(argv) == 0 {
		return Output{}, errors.New("no command")
	}
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Env = append(append(os.Environ(), "LC_ALL=C", "LANG=C"), env...)
	command.Dir = "/"
	command.WaitDelay = 10 * time.Second
	combined := lockedBuffer{live: live}
	command.Stdout, command.Stderr = &combined, &combined
	err := command.Run()
	output := Output{Text: combined.buffer.Bytes(), Streamed: live != nil}
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
