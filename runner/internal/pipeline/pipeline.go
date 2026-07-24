package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const maxOutputBytes = 16 * 1024

type Command struct {
	Command string
	Timeout time.Duration
}

type Result struct {
	Command  string
	ExitCode int
	Output   string
	Duration time.Duration
	TimedOut bool
}

type Runner interface {
	Run(context.Context, string, []Command) ([]Result, error)
}

type LocalRunner struct{}

func (LocalRunner) Run(ctx context.Context, workspace string, commands []Command) ([]Result, error) {
	results := make([]Result, 0, len(commands))
	for _, command := range commands {
		if err := validate(command); err != nil {
			return results, err
		}
		result := run(ctx, workspace, command)
		results = append(results, result)
		if result.TimedOut {
			return results, fmt.Errorf("pipeline command timed out: %s", command.Command)
		}
		if result.ExitCode != 0 {
			return results, fmt.Errorf("pipeline command failed with exit code %d: %s", result.ExitCode, command.Command)
		}
	}
	return results, nil
}

func validate(command Command) error {
	if _, err := ParseCommandTemplate(command.Command); err != nil {
		return err
	}
	if command.Timeout <= 0 {
		return errors.New("pipeline command timeout is invalid")
	}
	return nil
}

func ParseCommandTemplate(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" || strings.ContainsRune(value, 0) || strings.ContainsAny(value, "|&;<>`$()\\\n\r") {
		return nil, errors.New("pipeline command is invalid")
	}
	arguments := strings.Fields(value)
	if len(arguments) == 0 || len(arguments) > 64 {
		return nil, errors.New("pipeline command is invalid")
	}
	for _, argument := range arguments {
		if strings.ContainsAny(argument, "'\\\"") || len(argument) > 4096 {
			return nil, errors.New("pipeline command argument is invalid")
		}
	}
	return arguments, nil
}

func run(parent context.Context, workspace string, command Command) Result {
	ctx, cancel := context.WithTimeout(parent, command.Timeout)
	defer cancel()
	started := time.Now()
	arguments, err := ParseCommandTemplate(command.Command)
	if err != nil {
		return Result{Command: command.Command, ExitCode: -1, Duration: time.Since(started)}
	}
	process := exec.CommandContext(ctx, arguments[0], arguments[1:]...)
	process.Dir = workspace
	output, err := process.CombinedOutput()
	result := Result{Command: command.Command, ExitCode: 0, Output: truncateOutput(string(output)), Duration: time.Since(started)}
	if ctx.Err() == context.DeadlineExceeded {
		result.TimedOut = true
		result.ExitCode = -1
		return result
	}
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			result.ExitCode = exitError.ExitCode()
		} else {
			result.ExitCode = -1
		}
	}
	return result
}

func truncateOutput(output string) string {
	if len(output) <= maxOutputBytes {
		return output
	}
	return output[:maxOutputBytes]
}
