// Package execlog records what a command ran for the result's `output`: the
// combined stdout and stderr of each program, in order, after a `$ <argv>`
// line, as UTF-8 and capped at MaxBytes keeping the end.
package execlog

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// MaxBytes is the most output a command result carries.
const MaxBytes = 1 << 20

// Log collects output. It is safe for concurrent writes, as stdout and
// stderr of one program are.
type Log struct {
	mu        sync.Mutex
	buffer    []byte
	truncated bool
}

// Command starts the output of a program with its argv.
func (log *Log) Command(argv []string) {
	quoted := make([]string, len(argv))
	for index, argument := range argv {
		quoted[index] = quote(argument)
	}
	line := "$ " + strings.Join(quoted, " ") + "\n"
	log.mu.Lock()
	defer log.mu.Unlock()
	if len(log.buffer) > 0 && log.buffer[len(log.buffer)-1] != '\n' {
		log.buffer = append(log.buffer, '\n')
	}
	log.appendLocked([]byte(line))
}

// Write appends program output.
func (log *Log) Write(data []byte) (int, error) {
	log.mu.Lock()
	defer log.mu.Unlock()
	log.appendLocked(data)
	return len(data), nil
}

// appendLocked keeps at most twice MaxBytes in memory; Text trims to the
// final size at a character boundary.
func (log *Log) appendLocked(data []byte) {
	log.buffer = append(log.buffer, data...)
	if excess := len(log.buffer) - 2*MaxBytes; excess > 0 {
		log.buffer = append(log.buffer[:0], log.buffer[excess:]...)
		log.truncated = true
	}
}

// Text returns the output as valid UTF-8 of at most MaxBytes, and whether
// its beginning was cut off.
func (log *Log) Text() (string, bool) {
	log.mu.Lock()
	defer log.mu.Unlock()
	text, truncated := Cap(string(log.buffer))
	return text, truncated || log.truncated
}

// Cap makes output valid UTF-8 (invalid bytes become U+FFFD) and keeps at
// most MaxBytes of its end, starting at a character boundary.
func Cap(output string) (string, bool) {
	output = strings.ToValidUTF8(output, "�")
	if len(output) <= MaxBytes {
		return output, false
	}
	start := len(output) - MaxBytes
	for start < len(output) && !utf8.RuneStart(output[start]) {
		start++
	}
	return output[start:], true
}

// Run runs argv without a shell, writing its argv line and its combined
// output to log. A non-zero exit is reported as the exit code, not an
// error; failing to start (or ctx ending) is an error.
func Run(ctx context.Context, log *Log, argv []string) (int, error) {
	if len(argv) == 0 {
		return 0, errors.New("no command")
	}
	log.Command(argv)
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Stdout, command.Stderr = log, log
	command.WaitDelay = 10 * time.Second
	err := command.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && ctx.Err() == nil {
		return exit.ExitCode(), nil
	}
	if err != nil {
		_, _ = log.Write([]byte(err.Error() + "\n"))
		return -1, err
	}
	return 0, nil
}

// quote leaves plain arguments alone and single-quotes the rest, so the
// argv line reads like the command an administrator would type.
func quote(argument string) string {
	if argument != "" && strings.IndexFunc(argument, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./=:+,@%", r))
	}) < 0 {
		return argument
	}
	var builder bytes.Buffer
	builder.WriteByte('\'')
	builder.WriteString(strings.ReplaceAll(argument, "'", `'\''`))
	builder.WriteByte('\'')
	return builder.String()
}
