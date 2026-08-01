package control

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	runnerv1 "github.com/loop-engineering/contracts/gen/runner/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type secretClient struct {
	runnerv1.RunnerControlClient
	response *runnerv1.ResolveJobSecretResponse
	err      error
	requests []*runnerv1.ResolveJobSecretRequest
}

func (c *secretClient) ResolveJobSecret(_ context.Context, in *runnerv1.ResolveJobSecretRequest, _ ...grpc.CallOption) (*runnerv1.ResolveJobSecretResponse, error) {
	c.requests = append(c.requests, in)
	return c.response, c.err
}

func testResolver(t *testing.T, client *secretClient) *SecretResolver {
	t.Helper()
	return NewSecretResolver(client, Identity{RunnerID: "runner-1", Credential: "secret"}, t.TempDir())
}

func TestResolveReturnsAnEnvironmentValueWithoutTouchingDisk(t *testing.T) {
	client := &secretClient{response: &runnerv1.ResolveJobSecretResponse{Value: "ghp_x", Delivery: "environment"}}
	resolver := testResolver(t, client)

	value, path, err := resolver.Resolve(context.Background(), "job-1", 3, "GITHUB_TOKEN")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if value != "ghp_x" || path != "" {
		t.Fatalf("got value %q path %q, want the token and no path", value, path)
	}
	entries, _ := os.ReadDir(resolver.KeyDirectory)
	if len(entries) != 0 {
		t.Fatalf("an environment secret wrote %d files", len(entries))
	}
	request := client.requests[0]
	if request.GetJobId() != "job-1" || request.GetLeaseGeneration() != 3 || request.GetRunnerId() != "runner-1" {
		t.Fatalf("the request did not carry the job and lease: %+v", request)
	}
}

func TestAFileDeliveredSecretIsWrittenPrivatelyWithATrailingNewline(t *testing.T) {
	key := "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----"
	client := &secretClient{response: &runnerv1.ResolveJobSecretResponse{Value: key, Delivery: "file"}}
	resolver := testResolver(t, client)

	_, path, err := resolver.Resolve(context.Background(), "job-1", 1, "GIT_SSH_KEY")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	// ssh refuses a key any other user can read.
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode is %v, want 0600", info.Mode().Perm())
	}
	contents, _ := os.ReadFile(path)
	if !strings.HasSuffix(string(contents), "\n") {
		t.Fatal("ssh rejects PEM material with no trailing newline")
	}
	if strings.TrimSuffix(string(contents), "\n") != key {
		t.Fatal("the key was altered on the way to disk")
	}
}

func TestEveryRefusalTheRunnerShouldFallBackOnIsReportedAsSuch(t *testing.T) {
	for _, code := range []codes.Code{codes.NotFound, codes.FailedPrecondition, codes.InvalidArgument, codes.Unimplemented} {
		resolver := testResolver(t, &secretClient{err: status.Error(code, "no")})
		if _, _, err := resolver.Resolve(context.Background(), "job-1", 1, "GITHUB_TOKEN"); !errors.Is(err, ErrNoControlPlaneSecret) {
			t.Fatalf("%v produced %v, want ErrNoControlPlaneSecret", code, err)
		}
	}
}

func TestARealFailureIsNotMistakenForAnAbsentSecret(t *testing.T) {
	// Falling back here would run the job as the runner's own identity and
	// report itself as a permissions problem somewhere far from the cause.
	for _, code := range []codes.Code{codes.Unauthenticated, codes.Unavailable, codes.Internal} {
		resolver := testResolver(t, &secretClient{err: status.Error(code, "no")})
		_, _, err := resolver.Resolve(context.Background(), "job-1", 1, "GITHUB_TOKEN")
		if err == nil || errors.Is(err, ErrNoControlPlaneSecret) {
			t.Fatalf("%v produced %v, want a real error", code, err)
		}
	}
}

func TestDiscardRemovesOnlyTheNamedJobsKeys(t *testing.T) {
	client := &secretClient{response: &runnerv1.ResolveJobSecretResponse{Value: "key", Delivery: "file"}}
	resolver := testResolver(t, client)
	mine, _, err := resolveKey(t, resolver, "job-1")
	if err != nil {
		t.Fatal(err)
	}
	theirs, _, err := resolveKey(t, resolver, "job-2")
	if err != nil {
		t.Fatal(err)
	}

	if err := resolver.DiscardJobKeys("job-1"); err != nil {
		t.Fatalf("discard: %v", err)
	}
	if _, err := os.Stat(mine); !os.IsNotExist(err) {
		t.Fatal("the job's own key survived")
	}
	if _, err := os.Stat(theirs); err != nil {
		t.Fatal("a concurrent job's key was removed")
	}
}

func TestDiscardWithNoJobClearsEverythingLeftBehind(t *testing.T) {
	client := &secretClient{response: &runnerv1.ResolveJobSecretResponse{Value: "key", Delivery: "file"}}
	resolver := testResolver(t, client)
	if _, _, err := resolveKey(t, resolver, "job-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveKey(t, resolver, "job-2"); err != nil {
		t.Fatal(err)
	}

	// This is the startup sweep: a crash mid-job leaves key material behind,
	// and the next execution must not inherit one it was never granted.
	if err := resolver.DiscardJobKeys(""); err != nil {
		t.Fatalf("discard: %v", err)
	}
	entries, _ := os.ReadDir(resolver.KeyDirectory)
	if len(entries) != 0 {
		t.Fatalf("%d key files survived the startup sweep", len(entries))
	}
}

func TestDiscardOnAMissingDirectoryIsNotAnError(t *testing.T) {
	resolver := NewSecretResolver(&secretClient{}, Identity{RunnerID: "r", Credential: "c"}, filepath.Join(t.TempDir(), "absent"))
	if err := resolver.DiscardJobKeys(""); err != nil {
		t.Fatalf("discard: %v", err)
	}
}

func resolveKey(t *testing.T, resolver *SecretResolver, jobID string) (string, string, error) {
	t.Helper()
	_, path, err := resolver.Resolve(context.Background(), jobID, 1, "GIT_SSH_KEY")
	return path, "", err
}

func TestEnsureKeyDirectoryCreatesItWhenMissing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "moirai", "keys")
	resolver := NewSecretResolver(&secretClient{}, Identity{RunnerID: "r", Credential: "c"}, dir)

	if err := resolver.EnsureKeyDirectory(); err != nil {
		t.Fatalf("EnsureKeyDirectory() = %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Fatalf("directory was not created: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("the writability probe was left behind: %d entries", len(entries))
	}
}

// The shipped failure: /run is a root-owned tmpfs, the runner is unprivileged,
// and nothing noticed until a job was already in flight.
func TestEnsureKeyDirectoryFailsWhenItCannotBeWritten(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
	resolver := NewSecretResolver(&secretClient{}, Identity{RunnerID: "r", Credential: "c"},
		filepath.Join(parent, "keys"))

	err := resolver.EnsureKeyDirectory()
	if err == nil {
		t.Fatal("EnsureKeyDirectory() succeeded on an unwritable parent")
	}
	// The operator has to be told which directory, or the message sends them
	// looking through the whole container.
	if !strings.Contains(err.Error(), filepath.Join(parent, "keys")) {
		t.Fatalf("error = %q, want it to name the directory", err)
	}
}

func TestEnsureKeyDirectoryRejectsADirectoryOwnedByAnother(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	dir := filepath.Join(t.TempDir(), "keys")
	if err := os.MkdirAll(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	resolver := NewSecretResolver(&secretClient{}, Identity{RunnerID: "r", Credential: "c"}, dir)

	// Exists and MkdirAll succeeds, but nothing can be written into it -- which
	// is precisely the container case the probe exists to catch.
	if err := resolver.EnsureKeyDirectory(); err == nil {
		t.Fatal("EnsureKeyDirectory() accepted a directory it cannot write to")
	}
}
