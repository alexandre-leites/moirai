package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/loop-engineering/runner/internal/agents"
	"github.com/loop-engineering/runner/internal/repository"
	"github.com/loop-engineering/runner/internal/taskpacket"
)

// The most common autonomy failure is not a crash: it is an agent ending its
// own reasoning loop while the objective is still unmet — half the work done,
// "I will now do X" followed by an exit, or a clean refusal. A process exit is
// therefore not evidence that the goal was reached, and one terminal event per
// process exit reports "done" for runs that plainly are not.
//
// The goal gate is the deterministic answer to "was the objective met?", and
// the continuation loop is what acts on a "no" while the runner still holds the
// lease. Both live entirely inside one execution: the orchestrator sees one
// lease, one execution, and one terminal event, so no budget, protocol, or
// workflow state changes — only the terminal payload gains the continuation
// count and the gate's verdict on the attempt it reports.
//
// Deterministic gates still decide completion. Nothing here lets an agent
// declare its own success: the gate only ever withholds "delivered", and its
// evidence is the result document, the agent's own remaining-work list, and the
// repository diff.

// minimumContinuationRuntime is the least time a continuation is worth
// starting with. The packet's timeoutSeconds bounds the *total* agent wall
// clock of an execution, continuations included, so the last slice of that
// budget can be too small to do anything but launch a process and SIGTERM it.
//
// A packet that carries timeoutSeconds: 0 — what the orchestrator sends today,
// issue #276 — bounds nothing, and its shared deadline is already spent when
// the first attempt ends, so no continuation is ever funded.
const minimumContinuationRuntime = 5 * time.Second

// gateReason names one piece of missing evidence. The vocabulary is closed and
// carries no agent prose, no path, no exit code, and no timestamp, because it
// feeds the loop guard's signature: a verdict that differed run to run for
// incidental reasons would make a wedged agent look like a progressing one and
// the guard would never trip.
type gateReason string

const (
	reasonNoResultDocument gateReason = "no-result-document"
	reasonAgentNotComplete gateReason = "agent-not-complete"
	reasonAgentBlocked     gateReason = "agent-blocked"
	reasonRemainingWork    gateReason = "remaining-work"
	reasonNoChanges        gateReason = "no-changes"
)

func (reason gateReason) text() string {
	switch reason {
	case reasonNoResultDocument:
		return "no result document was written"
	case reasonAgentNotComplete:
		return "the agent run did not complete"
	case reasonAgentBlocked:
		return "the agent reported itself blocked"
	case reasonRemainingWork:
		return "the agent reported remaining work"
	case reasonNoChanges:
		return "no repository changes were made"
	}
	return string(reason)
}

// Why the continuation loop stopped. These are runner-owned strings, safe to
// report and stable across executions.
const (
	outcomeDelivered       = "delivered"
	outcomeBudget          = "continuation budget exhausted"
	outcomeRepeatedVerdict = "identical verdict repeated"
	outcomeTimeBudget      = "execution time budget exhausted"
	outcomeCancelled       = "execution cancelled"
	outcomeEvidenceHeld    = "previous evidence could not be set aside"
)

// gateVerdict is one evaluation of the goal gate.
type gateVerdict struct {
	// Delivered is true only when every check passed: a valid result document,
	// status "completed", no remaining work, and — for a role that may modify
	// files — a workspace change, meaning uncommitted changes or a moved HEAD.
	Delivered bool
	Reasons   []gateReason
	// Signature identifies this verdict for the loop guard. Two consecutive
	// runs producing the same signature are not making progress.
	Signature string
}

// attemptOutcome is what one agent invocation produced, paired with the gate's
// verdict on it.
type attemptOutcome struct {
	result  agents.Result
	err     error
	verdict gateVerdict
}

// rank separates an outcome the agent itself reached from one it did not.
//
// A continuation exists to improve on the attempt before it and must never be
// able to make it worse. Reporting only the last attempt would let a crashed or
// timed-out continuation replace a completed run's delivery, or — worse — an
// agent-declared block's stated reason (#97), with an anonymous failure: an
// outcome the very same execution reports correctly with the goal loop switched
// off. Every one of those destructive cases is an executor error — a signal, a
// timeout, or a clean exit with no result document — so an attempt that ended in
// one never outranks an attempt that did not. That is the same rule the terminal
// event applies: a failing process is not trusted to report its own outcome.
//
// Clean outcomes are deliberately *not* ranked against each other, and the later
// one wins. An agent that completes and then, asked to continue, declares itself
// blocked or failed has retracted its own claim with a stated reason, and that
// retraction is more truthful than the claim it replaced — it is also exactly
// the evidence a retry or an escalation is built from.
func (outcome attemptOutcome) rank() int {
	if outcome.err != nil {
		return 0
	}
	return 1
}

// agentRun is the outcome of an execution's whole agent phase: the best
// attempt's result and error, how many continuations were spent, and the gate's
// verdict on that attempt.
type agentRun struct {
	result        agents.Result
	err           error
	sessionID     string
	continuations int
	verdict       gateVerdict
	outcome       string
}

// report finishes the run with the attempt it will be judged on and the reason
// the loop stopped. The verdict travels with the attempt it was passed on, so a
// reported status and a reported verdict always describe the same invocation.
func (run agentRun) report(outcome attemptOutcome, why string) agentRun {
	run.result = outcome.result
	run.err = outcome.err
	run.verdict = outcome.verdict
	run.outcome = why
	return run
}

// verdictText renders the gate's verdict for the terminal payload, so the
// orchestrator can tell "delivered after 2 continuations" from "gave up after
// 3". It is built from the closed reason vocabulary and the runner's own
// outcome strings — never from agent prose — so it is bounded and stable. An
// empty signature means no verdict was ever reached: the budget is zero, or the
// execution was cancelled before the first evaluation.
func (run agentRun) verdictText() string {
	if run.verdict.Signature == "" {
		return ""
	}
	if run.verdict.Delivered {
		return outcomeDelivered
	}
	reasons := make([]string, 0, len(run.verdict.Reasons))
	for _, reason := range run.verdict.Reasons {
		reasons = append(reasons, reason.text())
	}
	if len(reasons) == 0 {
		return "not delivered (" + run.outcome + ")"
	}
	return fmt.Sprintf("not delivered (%s): %s", run.outcome, strings.Join(reasons, "; "))
}

// runAgent executes the agent and, while the goal gate rejects the run and a
// continuation budget remains, re-engages it in the same session.
//
// The loop terminates on five independent bounds, so it can neither run forever
// nor re-enter itself: the continuation count, the loop guard, the execution
// context, a result document that cannot be set aside, and the packet's own
// wall-clock deadline. It is a plain loop with no recursion and is called once
// per execution.
func (dispatcher Dispatcher) runAgent(
	ctx context.Context,
	generation int64,
	packet taskpacket.Packet,
	workspace repository.Workspace,
	initial repository.RevisionSummary,
	environment map[string]string,
	output io.Writer,
) agentRun {
	// The deadline is anchored at the first invocation and shared by every
	// continuation, which is what keeps the packet's timeoutSeconds a bound on
	// the *total* agent wall clock rather than a per-invocation allowance. The
	// first invocation still receives exactly the packet's timeout, so a run
	// that never continues behaves as it always did.
	timeout := time.Duration(packet.TimeoutSeconds) * time.Second
	deadline := time.Now().Add(timeout)
	// Resolved once and carried into every prompt this execution produces: the
	// environment does not change between continuations, and an agent that was
	// told what it has on the first attempt must not lose it on the second.
	declaration := dispatcher.declareEnvironment(workspace, packet)
	request := agents.Request{
		JobID:           packet.JobID,
		LeaseGeneration: generation,
		ExecutionID:     packet.ExecutionID,
		Role:            agents.Role(packet.Role),
		Workspace:       workspace.Repository,
		Prompt:          promptFor(packet) + declaration,
		ResultPath:      packet.ExpectedOutput,
		Timeout:         timeout,
		Environment:     environment,
		Output:          output,
	}
	var current attemptOutcome
	current.result, current.err = dispatcher.Backend.Execute(ctx, request)
	run := agentRun{result: current.result, err: current.err, sessionID: current.result.SessionID}
	budget := dispatcher.MaxContinuations
	if budget <= 0 {
		return run
	}
	best := current
	previousSignature := ""
	for {
		// A cancelled or expired lease must not be answered with another agent
		// process, and its workspace can no longer be measured either — the
		// gate's own snapshot runs on this context — so the run stops before it
		// can reach a verdict built on unmeasurable evidence.
		if ctx.Err() != nil {
			return run.report(best, outcomeCancelled)
		}
		current.verdict = dispatcher.evaluateGoalGate(ctx, packet, workspace, initial, current.result, current.err)
		if current.rank() >= best.rank() {
			best = current
		}
		if current.verdict.Delivered {
			return run.report(best, outcomeDelivered)
		}
		if run.continuations >= budget {
			return run.report(best, outcomeBudget)
		}
		// The loop guard. Two consecutive verdicts with the same missing
		// evidence, the same remaining work, and the same repository diff mean
		// the agent is wedged rather than progressing, and every further
		// continuation would spend the packet's time budget reproducing it.
		if current.verdict.Signature == previousSignature {
			return run.report(best, outcomeRepeatedVerdict)
		}
		remaining := time.Until(deadline)
		if remaining < minimumContinuationRuntime {
			return run.report(best, outcomeTimeBudget)
		}
		// The finished attempt's result document is set aside before the next
		// one runs, so the continuation is judged on evidence it produced
		// itself. A run that cannot be given a clean slate is not continued:
		// its verdict would be read off the previous attempt's document.
		if err := archiveResultDocument(workspace, packet, run.continuations+1); err != nil {
			slog.Warn("not continuing an agent whose previous result document could not be set aside",
				"job_id", packet.JobID, "execution_id", packet.ExecutionID, "error", err)
			return run.report(best, outcomeEvidenceHeld)
		}
		previousSignature = current.verdict.Signature
		run.continuations++
		continuation := request
		continuation.Prompt = continuationPrompt(packet, current.verdict, current.result, run.continuations, budget) + declaration
		continuation.SessionID = run.sessionID
		continuation.Timeout = remaining
		writeContinuationPrompt(workspace, packet, run.continuations, continuation.Prompt)
		slog.Info("runner is continuing an agent whose run did not meet the goal gate",
			"job_id", packet.JobID, "execution_id", packet.ExecutionID,
			"continuation", run.continuations, "max_continuations", budget,
			"gate_reasons", reasonText(current.verdict.Reasons), "resumed_session", continuation.SessionID != "")
		current = attemptOutcome{}
		current.result, current.err = dispatcher.Backend.Continue(ctx, continuation)
		if current.result.SessionID != "" {
			run.sessionID = current.result.SessionID
		}
	}
}

// archiveResultDocument moves a finished attempt's result document aside.
//
// It is what keeps a continuation honest. The document is written by the agent
// at a fixed path and validated only against the execution ID, which every
// attempt of an execution shares — so a continuation that exits without writing
// anything would otherwise be assessed against its predecessor's document and
// could be declared delivered on evidence it never produced. That is the "a
// clean exit is not a result" hole #89 closed, re-opened inside one execution.
//
// The document is renamed rather than deleted so a retained workspace still
// explains every attempt. An already-absent document is not an error: the
// attempt simply wrote none.
func archiveResultDocument(workspace repository.Workspace, packet taskpacket.Packet, attempt int) error {
	path, err := resultDocumentPath(workspace, packet)
	if err != nil {
		return err
	}
	archived := filepath.Join(filepath.Dir(path), fmt.Sprintf("result-attempt-%d.json", attempt))
	if err := os.Rename(path, archived); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("set aside the previous result document: %w", err)
	}
	return nil
}

// resultDocumentPath resolves the packet's result path inside the workspace.
// The packet validator already restricts it to `.loop/result.json`, but this
// runs os.Rename against the result, so the containment is re-checked here
// rather than assumed from a validation that happened elsewhere.
func resultDocumentPath(workspace repository.Workspace, packet taskpacket.Packet) (string, error) {
	if workspace.Repository == "" {
		return "", errors.New("workspace repository path is required")
	}
	relative := packet.ExpectedOutput
	if relative == "" {
		// The backends default to this path when the packet names none.
		relative = filepath.Join(".loop", "result.json")
	}
	if filepath.IsAbs(relative) || filepath.Clean(relative) != relative || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("result path %q is not inside the workspace", relative)
	}
	return filepath.Join(workspace.Repository, relative), nil
}

// evaluateGoalGate answers "was the objective met?" from evidence only.
//
//	(a) the result document is valid — the backends report an invalid or absent
//	    one as ErrNoResultEvidence rather than as a clean exit (#89);
//	(b) its status is "completed";
//	(c) its remainingWork is empty;
//	(d) for a role that may modify files, the workspace shows a change — either
//	    uncommitted changes or a HEAD the agent moved itself.
func (dispatcher Dispatcher) evaluateGoalGate(
	ctx context.Context,
	packet taskpacket.Packet,
	workspace repository.Workspace,
	initial repository.RevisionSummary,
	result agents.Result,
	executeErr error,
) gateVerdict {
	var verdict gateVerdict
	switch {
	case errors.Is(executeErr, agents.ErrNoResultEvidence):
		verdict.Reasons = append(verdict.Reasons, reasonNoResultDocument)
	case executeErr != nil || result.Status == "failed" || result.Status == "":
		verdict.Reasons = append(verdict.Reasons, reasonAgentNotComplete)
	case result.Status == "blocked":
		// A declared block is a deliberate stop with a stated reason, not a
		// crash, so it is worth one continuation: the blocker is sometimes
		// something the agent can clear once it is asked to. It is not worth
		// three — an agent that re-declares the same block produces the same
		// verdict, and the loop guard ends the run on the next pass.
		verdict.Reasons = append(verdict.Reasons, reasonAgentBlocked)
	case result.Status != "completed":
		verdict.Reasons = append(verdict.Reasons, reasonAgentNotComplete)
	}
	if len(result.RemainingWork) > 0 {
		verdict.Reasons = append(verdict.Reasons, reasonRemainingWork)
	}
	diff := dispatcher.diffEvidence(ctx, packet, workspace, initial, &verdict)
	verdict.Delivered = len(verdict.Reasons) == 0
	verdict.Signature = gateSignature(verdict.Reasons, result.RemainingWork, diff)
	return verdict
}

// diffEvidence applies check (d) and returns the diff's identity for the loop
// guard's signature.
//
// A role that may not modify files is expected to produce no diff, so the check
// does not apply to it — that is what keeps planners and reviewers out of the
// continuation loop. Without a revision inspector the runner has no diff
// evidence at all, the same condition Dispatcher.snapshot already treats as "no
// revision information"; the check is then skipped rather than failed, so a
// missing inspector cannot manufacture continuations. A snapshot that fails is
// reported and skipped for the same reason, and the execution's own final
// snapshot — which is not best effort — still fails the run if the repository
// is genuinely unreadable.
func (dispatcher Dispatcher) diffEvidence(
	ctx context.Context,
	packet taskpacket.Packet,
	workspace repository.Workspace,
	initial repository.RevisionSummary,
	verdict *gateVerdict,
) string {
	if !packet.Constraints.MayModifyFiles || dispatcher.RevisionInspector == nil {
		return ""
	}
	summary, err := dispatcher.RevisionInspector.Snapshot(ctx, workspace)
	if err != nil {
		slog.Warn("goal gate could not measure the repository diff; the change check was skipped",
			"job_id", packet.JobID, "execution_id", packet.ExecutionID, "error", err)
		return ""
	}
	if len(summary.ChangedFiles) == 0 && summary.Revision == initial.Revision {
		verdict.Reasons = append(verdict.Reasons, reasonNoChanges)
	}
	return diffSignature(summary)
}

// diffSignature identifies the state of the workspace for the loop guard. It is
// built from the revision and the sorted set of changed paths — the whole of
// what the existing snapshot machinery reports — so it is deterministic and
// free of timestamps and absolute paths.
//
// Two continuations that rewrite the same files with different content share a
// signature, so an agent editing and re-editing one file reads as no progress.
// That is the deliberate direction to err in: the guard exists to stop a run
// that is going nowhere, the continuation budget still bounds the case it
// misjudges, and every reason in the verdict has to match as well before it
// trips.
func diffSignature(summary repository.RevisionSummary) string {
	files := append([]string(nil), summary.ChangedFiles...)
	sort.Strings(files)
	return summary.Revision + "\x00" + strings.Join(files, "\x00")
}

// gateSignature hashes everything the loop guard compares. Remaining work is
// sorted before hashing so a re-ordered list is not mistaken for progress, and
// nothing else agent-written enters the signature.
func gateSignature(reasons []gateReason, remainingWork []string, diff string) string {
	remaining := append([]string(nil), remainingWork...)
	sort.Strings(remaining)
	hash := sha256.New()
	for _, reason := range reasons {
		fmt.Fprintf(hash, "reason\x00%s\x00", reason)
	}
	for _, entry := range remaining {
		fmt.Fprintf(hash, "remaining\x00%s\x00", entry)
	}
	fmt.Fprintf(hash, "diff\x00%s\x00", diff)
	return hex.EncodeToString(hash.Sum(nil))
}

func reasonText(reasons []gateReason) string {
	values := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		values = append(values, string(reason))
	}
	return strings.Join(values, ",")
}

// continuationPrompt tells the agent what is still missing and asks it to carry
// on rather than start again.
//
// It prepends its two sections to the role's whole original prompt rather than
// restating a summary of it. A resumed session would mostly cope with a summary;
// the fallback path would not, and that path is taken by every sessionless
// backend and by exactly the most common continuation of all — the one where no
// result document was written, so no session ID was ever captured. Those runs
// start a brand-new agent, and it must receive the same role framing, plan,
// prior failures, and review findings the first run did. For a reviewer that
// framing is also what keeps the role independent, which a generic continuation
// prompt would quietly drop.
func continuationPrompt(packet taskpacket.Packet, verdict gateVerdict, result agents.Result, attempt, budget int) string {
	return strings.Join([]string{
		"# CONTINUATION",
		fmt.Sprintf(
			"This is continuation %d of at most %d for execution %s. Your previous run stopped before the objective was met. Continue from where you left off: keep the work you already did, do not start over, and do not repeat completed steps. The full task follows below and is unchanged.",
			attempt, budget, packet.ExecutionID),
		"# MISSING EVIDENCE", listText(missingEvidence(packet, verdict, result), "The objective is not yet evidenced as met."),
	}, "\n\n") + "\n\n" + promptFor(packet)
}

// missingEvidence states, in the agent's terms, what the gate did not find.
// Unlike the verdict text this is written for the agent rather than for the
// orchestrator, so it does quote the agent's own summary and remaining-work
// entries back at it — that is the specific instruction it needs.
//
// Everything quoted back is bounded first, with the same helpers the terminal
// payload uses. Nothing limits how much an agent writes into its result
// document, and the whole prompt travels as a single argv element, so an
// unbounded quotation would eventually fail the exec outright and spend a
// continuation on a process that never started.
func missingEvidence(packet taskpacket.Packet, verdict gateVerdict, result agents.Result) []string {
	evidence := make([]string, 0, len(verdict.Reasons))
	for _, reason := range verdict.Reasons {
		switch reason {
		case reasonNoResultDocument:
			evidence = append(evidence, fmt.Sprintf("No result document was written at %s. Write it before you finish.", defaultText(packet.ExpectedOutput, filepath.Join(".loop", "result.json"))))
		case reasonAgentNotComplete:
			evidence = append(evidence, "The previous run ended without a completed result. Finish the work and report it.")
		case reasonAgentBlocked:
			blocker := defaultText(boundedAgentText(result.Summary, maxTerminalPayloadFieldBytes), "no reason was given")
			evidence = append(evidence, fmt.Sprintf("The previous run reported itself blocked: %s. Clear the blocker if you can; report the block again only if it is genuinely outside your control.", blocker))
		case reasonRemainingWork:
			evidence = append(evidence, "You listed work as still remaining: "+strings.Join(boundedList(result.RemainingWork), "; ")+". Complete it, then leave remainingWork empty.")
		case reasonNoChanges:
			evidence = append(evidence, "No file in the repository was changed, and this role is expected to modify files. Make the changes the objective requires.")
		}
	}
	return evidence
}

// writeContinuationPrompt keeps each continuation prompt beside the first one
// so a retained workspace explains why the execution continued and what the
// agent was asked. It is best effort: a run must not fail because a forensic
// artifact could not be written.
func writeContinuationPrompt(workspace repository.Workspace, packet taskpacket.Packet, attempt int, prompt string) {
	if workspace.Repository == "" || packet.PromptPath == "" {
		return
	}
	path := filepath.Join(workspace.Repository, filepath.Dir(packet.PromptPath), fmt.Sprintf("continuation-%d.md", attempt))
	if err := os.WriteFile(path, []byte(prompt), 0o600); err != nil {
		slog.Warn("could not record the continuation prompt",
			"job_id", packet.JobID, "execution_id", packet.ExecutionID, "continuation", attempt, "error", err)
	}
}
