package control

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	runnerv1 "github.com/loop-engineering/contracts/gen/runner/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// SecretResolver asks the orchestrator for the credentials a job needs, so the
// runner host does not have to be provisioned with them.
//
// The orchestrator refuses to answer over an insecure channel, and a project
// may simply have no credential of its own. Both come back as a refusal the
// caller treats the same way: fall back to whatever the runner has in its own
// environment. That is what keeps an existing deployment working unchanged.
type SecretResolver struct {
	client     runnerv1.RunnerControlClient
	runnerID   string
	credential string
	// KeyDirectory is where a "file" delivery is written. It must be on a
	// filesystem the runner can write and that does not survive a reboot --
	// /run/moirai/keys on the container's tmpfs.
	KeyDirectory string
}

func NewSecretResolver(client runnerv1.RunnerControlClient, identity Identity, keyDirectory string) *SecretResolver {
	return &SecretResolver{
		client:       client,
		runnerID:     identity.RunnerID,
		credential:   identity.Credential,
		KeyDirectory: keyDirectory,
	}
}

// ErrNoControlPlaneSecret means the control plane declined to provide this
// secret and the caller should look elsewhere. It deliberately does not say
// which refusal it was: "this project has none", "your lease is stale" and
// "this deployment has no TLS" all lead to the same fallback, and the runner
// has no business acting on the difference.
var ErrNoControlPlaneSecret = errors.New("the control plane has no secret for this reference")

// Resolve returns the secret's value, and the path it was written to when the
// orchestrator asked for file delivery.
func (r *SecretResolver) Resolve(ctx context.Context, jobID string, generation int64, name string) (value string, path string, err error) {
	if r == nil || r.client == nil {
		return "", "", ErrNoControlPlaneSecret
	}
	response, err := r.client.ResolveJobSecret(ctx, &runnerv1.ResolveJobSecretRequest{
		RunnerId:        r.runnerID,
		Credential:      r.credential,
		JobId:           jobID,
		LeaseGeneration: generation,
		Name:            name,
	})
	if err != nil {
		switch status.Code(err) {
		case codes.NotFound, codes.FailedPrecondition, codes.InvalidArgument, codes.Unimplemented:
			// Unimplemented covers an orchestrator older than this runner.
			return "", "", ErrNoControlPlaneSecret
		default:
			// Unauthenticated, Unavailable and anything else is a real problem.
			// Falling back would run the job as the wrong identity and report
			// itself as a permissions failure somewhere far from here.
			return "", "", fmt.Errorf("resolve %s from the control plane: %w", name, err)
		}
	}
	if response.GetValue() == "" {
		return "", "", ErrNoControlPlaneSecret
	}
	if response.GetDelivery() != "file" {
		return response.GetValue(), "", nil
	}
	written, err := r.writeKey(jobID, name, response.GetValue())
	if err != nil {
		return "", "", err
	}
	return response.GetValue(), written, nil
}

// writeKey materialises a value ssh can read. ssh takes a path, not a variable,
// and refuses a key file other users can read -- hence 0600 in a directory only
// this runner owns.
func (r *SecretResolver) writeKey(jobID, name, value string) (string, error) {
	if r.KeyDirectory == "" {
		return "", errors.New("no key directory is configured for file-delivered secrets")
	}
	if err := os.MkdirAll(r.KeyDirectory, 0o700); err != nil {
		return "", fmt.Errorf("create key directory: %w", err)
	}
	path := filepath.Join(r.KeyDirectory, jobID+"."+name)
	// O_EXCL would fail a retry of the same job; truncating is correct here
	// because the value is re-fetched every time and the old one is dead.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("create key file: %w", err)
	}
	defer file.Close()
	// ssh wants a trailing newline on PEM material and rejects a key without
	// one with a misleading "invalid format".
	if _, err := file.WriteString(ensureTrailingNewline(value)); err != nil {
		return "", fmt.Errorf("write key file: %w", err)
	}
	return path, nil
}

func ensureTrailingNewline(value string) string {
	if value == "" || value[len(value)-1] == '\n' {
		return value
	}
	return value + "\n"
}

// DiscardJobKeys removes any key files written for a job. Called when the job
// ends, and for the whole directory at startup, so a crash mid-job cannot leave
// private key material behind for the next one.
func (r *SecretResolver) DiscardJobKeys(jobID string) error {
	if r == nil || r.KeyDirectory == "" {
		return nil
	}
	entries, err := os.ReadDir(r.KeyDirectory)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read key directory: %w", err)
	}
	var failures []error
	for _, entry := range entries {
		if jobID != "" && !matchesJob(entry.Name(), jobID) {
			continue
		}
		if err := os.Remove(filepath.Join(r.KeyDirectory, entry.Name())); err != nil && !os.IsNotExist(err) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func matchesJob(entryName, jobID string) bool {
	return len(entryName) > len(jobID) && entryName[:len(jobID)] == jobID && entryName[len(jobID)] == '.'
}
