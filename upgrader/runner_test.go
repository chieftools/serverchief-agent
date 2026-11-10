package upgrader

import (
	"context"
	"testing"
)

// systemd stopping the upgrade's unit kills apt with SIGTERM; the output
// says so instead of "exit status -1".
func TestExecRunnerNamesTheSignalThatKilledTheProgram(t *testing.T) {
	output, err := ExecRunner(context.Background(), nil, []string{"/bin/sh", "-c", "kill -TERM $$"}, nil)

	if err != nil || output.ExitCode != -1 || output.Signal != "SIGTERM" || output.Failure() != "was killed by SIGTERM" {
		t.Fatalf("output = %+v, error %v", output, err)
	}
}

func TestExecRunnerReportsTheExitStatus(t *testing.T) {
	output, err := ExecRunner(context.Background(), nil, []string{"/bin/sh", "-c", "exit 100"}, nil)

	if err != nil || output.ExitCode != 100 || output.Signal != "" || output.Failure() != "exited with status 100" {
		t.Fatalf("output = %+v, error %v", output, err)
	}
}
