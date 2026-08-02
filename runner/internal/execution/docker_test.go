package execution

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDockerExecutorBuildsRestrictedRunCommand(t *testing.T) {
	workspace := t.TempDir()
	binary, recorded := fakeDocker(t)
	executor := DockerExecutor{
		Binary:      binary,
		Image:       "example/agent:1",
		CPULimit:    "2",
		MemoryLimit: "1g",
	}
	var output bytes.Buffer
	_, err := executor.Execute(context.Background(), Request{
		ExecutionID: "execution-1",
		Workspace:   workspace,
		Command:     []string{"/bin/sh", "-c", "true"},
		Environment: map[string]string{"B": "second", "A": "first"},
		Timeout:     time.Second,
	}, &output, &output)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	arguments := readDockerArguments(t, recorded)
	want := []string{
		"run", "--rm", "--init", "--name", dockerContainerName("execution-1"),
		"--workdir", "/workspace", "--mount", "type=bind,src=" + workspace + ",dst=/workspace",
		"--network", "bridge", "--cpus", "2", "--memory", "1g", "--env-file",
	}
	if len(arguments) != len(want)+5 || strings.Join(arguments[:len(want)], "\n") != strings.Join(want, "\n") || strings.Contains(strings.Join(arguments, "\n"), "A=first") || strings.Contains(strings.Join(arguments, "\n"), "B=second") {
		t.Fatalf("docker arguments = %#v", arguments)
	}
}

func TestDockerExecutorMountsDeliveredSSHKeyReadOnly(t *testing.T) {
	key := filepath.Join(t.TempDir(), "key")
	arguments := DockerExecutor{Image: "example/agent:1"}.runCommand("container", t.TempDir(), Request{Environment: map[string]string{"GIT_SSH_KEY": key}}, "")
	want := "type=bind,src=" + key + ",dst=" + key + ",readonly"
	if !strings.Contains(strings.Join(arguments, "\n"), want) {
		t.Fatalf("docker arguments = %#v, want SSH key mount", arguments)
	}
}

func TestDockerExecutorRejectsUnsafeConfiguration(t *testing.T) {
	executor := DockerExecutor{Image: "example image"}
	_, err := executor.Execute(context.Background(), Request{
		ExecutionID: "execution-1",
		Workspace:   t.TempDir(),
		Command:     []string{"true"},
		Timeout:     time.Second,
	}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "image") {
		t.Fatalf("Execute() error = %v, want image validation error", err)
	}

	executor.Image = "example/agent:1"
	_, err = executor.Execute(context.Background(), Request{
		ExecutionID: "execution-1",
		Workspace:   t.TempDir(),
		Command:     []string{"true"},
		Environment: map[string]string{"BAD=NAME": "value"},
		Timeout:     time.Second,
	}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "environment") {
		t.Fatalf("Execute() error = %v, want environment validation error", err)
	}
}

func TestDockerExecutorTimeoutStopsContainer(t *testing.T) {
	workspace := t.TempDir()
	binary, recorded := fakeDocker(t)
	executor := DockerExecutor{Binary: binary, Image: "example/agent:1"}
	_, err := executor.Execute(context.Background(), Request{
		ExecutionID: "execution-timeout",
		Workspace:   workspace,
		Command:     []string{"sleep"},
		Timeout:     25 * time.Millisecond,
	}, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Execute() error = %v, want deadline exceeded", err)
	}
	if !strings.Contains(strings.Join(readDockerArguments(t, recorded), "\n"), "stop\n--time\n10\n"+dockerContainerName("execution-timeout")) {
		t.Fatalf("expected Docker stop command, got %q", readDockerArguments(t, recorded))
	}
}

func TestDockerExecutorBoundsStopAfterExecutionTimeout(t *testing.T) {
	workspace := t.TempDir()
	binary := blockingStopDocker(t)
	executor := DockerExecutor{
		Binary:      binary,
		Image:       "example/agent:1",
		StopTimeout: 25 * time.Millisecond,
	}

	started := time.Now()
	_, err := executor.Execute(context.Background(), Request{
		ExecutionID: "execution-blocking-stop",
		Workspace:   workspace,
		Command:     []string{"sleep"},
		Timeout:     25 * time.Millisecond,
	}, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Execute() error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Execute() took %s, want bounded stop timeout", elapsed)
	}
}

func blockingStopDocker(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	binary := filepath.Join(directory, "docker")
	script := "#!/bin/sh\nif [ \"$1\" = stop ]; then sleep 10; fi\nfor argument in \"$@\"; do if [ \"$argument\" = sleep ]; then sleep 10; fi; done\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatalf("write blocking fake docker: %v", err)
	}
	return binary
}

func fakeDocker(t *testing.T) (string, string) {
	t.Helper()
	directory := t.TempDir()
	recorded := filepath.Join(directory, "arguments")
	binary := filepath.Join(directory, "docker")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"" + recorded + "\"\nfor argument in \"$@\"; do if [ \"$argument\" = sleep ]; then sleep 10; fi; done\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	t.Setenv("LOOP_DOCKER_ARGS", recorded)
	return binary, recorded
}

func readDockerArguments(t *testing.T, path string) []string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read recorded Docker arguments: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(contents), "\n"), "\n")
}
