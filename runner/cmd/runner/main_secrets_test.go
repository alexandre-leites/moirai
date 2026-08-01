package main

import (
	"context"
	"os"
	"strings"
	"testing"

	runnerv1 "github.com/loop-engineering/contracts/gen/runner/v1"
	"github.com/loop-engineering/runner/internal/control"
	"github.com/loop-engineering/runner/internal/dispatch"
	"github.com/loop-engineering/runner/internal/taskpacket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type stubSecretClient struct {
	runnerv1.RunnerControlClient
	byName map[string]*runnerv1.ResolveJobSecretResponse
	err    error
	asked  []string
}

func (c *stubSecretClient) ResolveJobSecret(_ context.Context, in *runnerv1.ResolveJobSecretRequest, _ ...grpc.CallOption) (*runnerv1.ResolveJobSecretResponse, error) {
	c.asked = append(c.asked, in.GetName())
	if c.err != nil {
		return nil, c.err
	}
	if response, ok := c.byName[in.GetName()]; ok {
		return response, nil
	}
	return nil, status.Error(codes.NotFound, "no credential")
}

func newResolver(t *testing.T, client *stubSecretClient) controlPlaneResolver {
	t.Helper()
	return controlPlaneResolver{
		secrets: control.NewSecretResolver(client, control.Identity{RunnerID: "r", Credential: "c"}, t.TempDir()),
	}
}

var githubRef = []taskpacket.EnvironmentRef{{Name: "GITHUB_TOKEN", SecretRef: "github_token"}}

func TestTheProjectsOwnCredentialIsPreferredOverTheRunnersEnvironment(t *testing.T) {
	// This is the whole point of the feature: a runner provisioned with a token
	// that cannot see a private repository must still use the project's.
	t.Setenv("GITHUB_TOKEN", "ghp_runner_wide")
	resolver := newResolver(t, &stubSecretClient{byName: map[string]*runnerv1.ResolveJobSecretResponse{
		"GITHUB_TOKEN": {Value: "ghp_project", Delivery: "environment"},
	}})

	resolved, err := resolver.Resolve(context.Background(), dispatch.SecretScope{JobID: "job-1", LeaseGeneration: 1}, githubRef)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved["GITHUB_TOKEN"] != "ghp_project" {
		t.Fatalf("resolved %q, want the project's credential", resolved["GITHUB_TOKEN"])
	}
}

func TestARunnerFallsBackToItsOwnEnvironmentWhenTheProjectHasNoCredential(t *testing.T) {
	// An existing deployment has no per-project credentials and no TLS. It has
	// to keep working exactly as it did.
	t.Setenv("GITHUB_TOKEN", "ghp_runner_wide")
	resolver := newResolver(t, &stubSecretClient{err: status.Error(codes.FailedPrecondition, "insecure channel")})

	resolved, err := resolver.Resolve(context.Background(), dispatch.SecretScope{JobID: "job-1", LeaseGeneration: 1}, githubRef)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved["GITHUB_TOKEN"] != "ghp_runner_wide" {
		t.Fatalf("resolved %q, want the runner's own token", resolved["GITHUB_TOKEN"])
	}
}

func TestAnUnreachableControlPlaneFailsRatherThanQuietlyUsingTheWrongIdentity(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "ghp_runner_wide")
	resolver := newResolver(t, &stubSecretClient{err: status.Error(codes.Unavailable, "no route")})

	if _, err := resolver.Resolve(context.Background(), dispatch.SecretScope{JobID: "job-1", LeaseGeneration: 1}, githubRef); err == nil {
		t.Fatal("Resolve() succeeded, want the transport failure surfaced")
	}
}

func TestAMissingCredentialEverywhereIsReportedNotSilentlyOmitted(t *testing.T) {
	os.Unsetenv("GITHUB_TOKEN")
	resolver := newResolver(t, &stubSecretClient{})

	_, err := resolver.Resolve(context.Background(), dispatch.SecretScope{JobID: "job-1", LeaseGeneration: 1}, githubRef)
	if err == nil {
		t.Fatal("Resolve() succeeded with no credential anywhere")
	}
	// The operator has to be told which reference, or the message sends them
	// looking through every credential the deployment has.
	if !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Fatalf("error = %q, want it to name the unresolved reference", err)
	}
}

func TestAnSSHKeyIsHandedOnAsAPathNotAsKeyMaterial(t *testing.T) {
	// The key must never become an environment variable: every process the
	// agent spawns would inherit it.
	key := "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----"
	resolver := newResolver(t, &stubSecretClient{byName: map[string]*runnerv1.ResolveJobSecretResponse{
		"GIT_SSH_KEY": {Value: key, Delivery: "file"},
	}})

	resolved, err := resolver.Resolve(
		context.Background(),
		dispatch.SecretScope{JobID: "job-1", LeaseGeneration: 1},
		[]taskpacket.EnvironmentRef{{Name: "GIT_SSH_KEY", SecretRef: "ssh_private_key"}},
	)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved["GIT_SSH_KEY"] == key {
		t.Fatal("the key material itself was put in the environment")
	}
	contents, readErr := os.ReadFile(resolved["GIT_SSH_KEY"])
	if readErr != nil {
		t.Fatalf("GIT_SSH_KEY is not a readable path: %v", readErr)
	}
	if string(contents) != key+"\n" {
		t.Fatalf("the file holds %q", contents)
	}
}

func TestEachReferenceIsResolvedIndependently(t *testing.T) {
	// A project may carry a token and no key, or the reverse. One missing
	// credential must not drag the other into the fallback.
	t.Setenv("GIT_SSH_KEY", "/run/keys/from-the-runner")
	resolver := newResolver(t, &stubSecretClient{byName: map[string]*runnerv1.ResolveJobSecretResponse{
		"GITHUB_TOKEN": {Value: "ghp_project", Delivery: "environment"},
	}})

	resolved, err := resolver.Resolve(
		context.Background(),
		dispatch.SecretScope{JobID: "job-1", LeaseGeneration: 1},
		[]taskpacket.EnvironmentRef{
			{Name: "GITHUB_TOKEN", SecretRef: "github_token"},
			{Name: "GIT_SSH_KEY", SecretRef: "ssh_private_key"},
		},
	)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if resolved["GITHUB_TOKEN"] != "ghp_project" {
		t.Fatalf("token = %q, want the project's", resolved["GITHUB_TOKEN"])
	}
	if resolved["GIT_SSH_KEY"] != "/run/keys/from-the-runner" {
		t.Fatalf("key = %q, want the runner's own", resolved["GIT_SSH_KEY"])
	}
}
