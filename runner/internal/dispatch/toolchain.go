package dispatch

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/loop-engineering/runner/internal/agents"
	"github.com/loop-engineering/runner/internal/repository"
	"github.com/loop-engineering/runner/internal/taskpacket"
	"github.com/loop-engineering/runner/internal/toolchain"
)

// The agent's prompt is assembled in two stages, and this is the second one.
//
// writeArtifacts writes what the control plane knows: the objective, the issue,
// the plan, the constraints -- everything that was decided before the job was
// offered. What it cannot write is which environment the agent will be launched
// into, because that is chosen here, on the runner: a packet may name an
// execution image of its own (#219), the operator may have configured a
// container backend, or the agent may run directly in the runner image. So the
// environment declares itself, and the runner appends that declaration to the
// prompt at the point the environment is bound.
//
// The pipeline role never reaches this code, which is right: it runs commands,
// not an agent, and has no prompt to read.
const environmentSectionHeading = "# EXECUTION ENVIRONMENT"

// toolchainManifestPath is where the runner looks for the declaration of the
// environment it is about to launch an agent in. Overridden only by tests; it
// is a convention rather than a setting, so that every image answers the same
// question at the same place.
var toolchainManifestPath = toolchain.DefaultManifestPath

// declareEnvironment returns the prompt section describing the execution
// environment, and appends it to the prompt artifact on disk -- which is the
// copy the agent actually reads when its harness is pointed at
// `.loop/prompt.md` rather than handed the prompt on a command line.
//
// The section is returned rather than only written so that the caller can carry
// it into every prompt of the execution, continuations included, and so the
// environment is resolved once per execution rather than once per attempt.
//
// It never fails a run. An environment with nothing to declare returns "", and
// the prompt is exactly what it was before this existed.
func (dispatcher Dispatcher) declareEnvironment(workspace repository.Workspace, packet taskpacket.Packet) string {
	declaration := dispatcher.environmentDeclaration(packet)
	if declaration == "" {
		return ""
	}
	section := "\n\n" + environmentSectionHeading + "\n\n" + declaration
	appendPromptSection(workspace, packet, section)
	return section
}

// environmentDeclaration renders what is known about the environment the agent
// will run in, or "" when nothing is.
//
// The distinction that matters is whether the agent runs in this runner's own
// filesystem. When it does, the manifest here describes it and is read
// directly. When the agent is launched into a container image instead, this
// filesystem describes the wrong machine -- declaring it would be worse than
// declaring nothing -- so the agent is pointed at the manifest inside the image
// it is actually in. That is still a declaration rather than a discovery: one
// cheap read at a known path, instead of finding each absence by running a
// command that fails.
func (dispatcher Dispatcher) environmentDeclaration(packet taskpacket.Packet) string {
	if image := agentImage(dispatcher.Backend, packet); image != "" {
		return fmt.Sprintf(
			"You are running inside the container image `%s`, not on the runner host.\n\n"+
				"That image declares the toolchain it offers you at `%s`. Read that file first: it lists what is installed and, just as importantly, what is not. If it is absent the image predates the convention and you will have to check tools yourself with `command -v`.\n\n"+
				"Do not discover the toolchain by running a command and seeing it fail. A failed command still costs a full attempt, and the budget is two.\n",
			image, toolchain.DefaultManifestPath,
		)
	}
	manifest, err := toolchain.Load(toolchainManifestPath)
	switch {
	case err == nil:
		return manifest.Declaration()
	case errors.Is(err, toolchain.ErrNoManifest):
		// Normal: a runner on a bare host, or an image built before the
		// convention. The agent is told nothing rather than something wrong.
		slog.Debug("execution environment declares no toolchain to the agent",
			"job_id", packet.JobID, "execution_id", packet.ExecutionID, "manifest", toolchainManifestPath)
	default:
		// A manifest that exists but cannot be read is a defect in the image,
		// and a silent one: the agent goes back to probing and nothing says why.
		slog.Warn("execution environment publishes an unusable toolchain manifest",
			"job_id", packet.JobID, "execution_id", packet.ExecutionID, "manifest", toolchainManifestPath, "error", err)
	}
	return ""
}

// agentImage names the container image the agent will be launched into, or ""
// when it runs in the runner's own filesystem.
//
// The packet's execution image is authoritative because Execute has already
// rebuilt the backend around it. The backend is consulted second for the
// operator-configured container backend, which no packet field describes.
func agentImage(backend agents.Backend, packet taskpacket.Packet) string {
	if packet.ExecutionImage != "" {
		return packet.ExecutionImage
	}
	if docker, ok := backend.(agents.DockerCLIBackend); ok {
		return docker.Image
	}
	return ""
}

// appendPromptSection adds the environment declaration to the prompt artifact
// writeArtifacts already wrote. Appending rather than rewriting keeps the two
// stages separable: whatever the control plane asked for is still on disk
// exactly as it was sent, with the runner's own contribution after it.
//
// Best effort, like the continuation prompts beside it. A prompt artifact that
// cannot be extended is a degraded agent, not a failed job -- and the backends
// that take the prompt on their command line get the section either way.
func appendPromptSection(workspace repository.Workspace, packet taskpacket.Packet, section string) {
	if workspace.Repository == "" || packet.PromptPath == "" {
		return
	}
	path := filepath.Join(workspace.Repository, packet.PromptPath)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err == nil {
		_, err = file.WriteString(section)
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		slog.Warn("could not declare the execution environment in the agent prompt",
			"job_id", packet.JobID, "execution_id", packet.ExecutionID, "error", err)
	}
}
