// Runners (specification.md §5.5): the fleet that executes what the orchestrator
// schedules, plus the one-time tokens that let a new runner join it.
import { useCallback, useState } from "react";
import type { ApiClient, CreatedToken, Runner, RunnerToken } from "./api";
import { ApiError } from "./api";
import { useConsoleData } from "./console-data";
import { absolute } from "./format";
import { usePolled } from "./poll";
import { describeHeartbeat, describeRunnerStatus } from "./runner-status";
import { useIsAdmin } from "./auth";
import {
  Card, CardHeader, Empty, ErrorBlock, KV, KVRow, Modal, Pill, Skeleton, TableWrap,
  useConfirm, useToast,
} from "./ui";

export function RunnersPage({ api }: { api: ApiClient }) {
  const isAdmin = useIsAdmin();
  const toast = useToast();
  const { confirm, dialog } = useConfirm();
  const { data, error, loading, refresh } = useConsoleData();
  const [busyId, setBusyId] = useState<string | null>(null);

  // Heartbeat ages are measured against when the snapshot was requested, not
  // when it arrived: adding the round-trip would report a healthy fleet as stale
  // exactly when the orchestrator is slow.
  const [now] = useState(() => Date.now());

  const setState = (runner: Runner, state: "drain" | "enable" | "revoke", success: string) => {
    setBusyId(runner.id);
    api.setRunnerState(runner.id, state).then(
      () => { toast(success); refresh(); },
      (reason: unknown) => {
        toast(reason instanceof ApiError && reason.isForbidden
          ? "You need the admin role for that"
          : `Could not update ${runner.name}: ${reason instanceof Error ? reason.message : String(reason)}`);
      }
    ).finally(() => setBusyId(null));
  };

  return (
    <div>
      <div className="view-head"><h1>Runners</h1></div>
      <p className="view-sub">
        The fleet that executes what the orchestrator schedules. A runner takes offers only while it is
        online, enabled, and not draining.
      </p>

      {error && <ErrorBlock title={data ? "Showing the last good snapshot — the refresh failed." : "The fleet could not be loaded."} detail={error} onRetry={refresh} />}
      {loading && !data && <Skeleton cards={2} />}

      {data && (
        <>
          {data.runners.length === 0 ? (
            <Card>
              <Empty>
                No runner is registered. Issue a registration token below, then start a runner against this
                orchestrator to see it here.
              </Empty>
            </Card>
          ) : (
            <div className="grid cards-auto gap-b">
              {data.runners.map((runner) => {
                const heartbeat = describeHeartbeat(runner.lastSeenAt, now);
                const status = describeRunnerStatus(runner, heartbeat);
                // Which run a runner is holding needs the runner id on the
                // workflow payload — specification task A12.
                return (
                  <Card key={runner.id}>
                    <div className="card-h">
                      <h2 className="num">{runner.name}</h2>
                      <Pill variant={status.variant} pulse={status.variant === "ok"}>{status.label}</Pill>
                    </div>
                    <div className="card-b">
                      <KV>
                        <KVRow label="Heartbeat">
                          <span className="num" title={absolute(runner.lastSeenAt) ?? heartbeat.title}>{heartbeat.label}</span>
                        </KVRow>
                        <KVRow label="Labels">
                          {runner.labels.length === 0
                            ? <span className="t2">any</span>
                            : runner.labels.map((label) => <Pill key={label} variant="idle" dot={false}>{label}</Pill>)}
                        </KVRow>
                        <KVRow label="Accepting offers">
                          {runner.enabled && !runner.draining ? "yes" : <span className="t2">no</span>}
                        </KVRow>
                        <KVRow label="Runner id"><span className="num t2">{runner.id}</span></KVRow>
                      </KV>
                      {runner.draining && (
                        <p className="note-warn">
                          Draining — finishes its current work, then accepts no offers.
                        </p>
                      )}
                      {isAdmin && (
                        <div className="btnrow">
                          <button
                            type="button"
                            className="btn sm"
                            disabled={busyId === runner.id}
                            onClick={() => runner.draining
                              ? setState(runner, "enable", "Drain cancelled — the runner accepts offers again")
                              : setState(runner, "drain", "Draining — finishes current work, then accepts no offers")}
                          >
                            {runner.draining ? "Stop draining" : "Drain"}
                          </button>
                          <button
                            type="button"
                            className="btn sm danger"
                            disabled={busyId === runner.id}
                            onClick={() => confirm({
                              title: `Revoke ${runner.name}`,
                              body: "Its credential is invalidated and its stream is closed. The runner has to register again with a new token.",
                              confirmLabel: "Revoke runner",
                              danger: true,
                              onConfirm: () => setState(runner, "revoke", "Runner revoked — credential invalidated"),
                            })}
                          >
                            Revoke
                          </button>
                        </div>
                      )}
                    </div>
                  </Card>
                );
              })}
            </div>
          )}
        </>
      )}

      <Tokens api={api} />
      {dialog}
    </div>
  );
}

// --- Registration tokens --------------------------------------------------

function Tokens({ api }: { api: ApiClient }) {
  const isAdmin = useIsAdmin();
  const toast = useToast();
  const { confirm, dialog } = useConfirm();
  const [issued, setIssued] = useState<CreatedToken | null>(null);
  const [creating, setCreating] = useState(false);
  const [labels, setLabels] = useState("");
  const [form, setForm] = useState(false);

  const load = useCallback((signal: AbortSignal) => api.listTokens(signal), [api]);
  const { data, error, refresh } = usePolled(load, "Registration tokens are unavailable.");

  const create = () => {
    setCreating(true);
    const allowed = labels.split(",").map((label) => label.trim()).filter(Boolean);
    api.createToken(allowed).then(
      (token) => { setIssued(token); setForm(false); setLabels(""); refresh(); },
      (reason: unknown) => {
        toast(reason instanceof ApiError && reason.isForbidden
          ? "You need the admin role for that"
          : `Could not create a token: ${reason instanceof Error ? reason.message : String(reason)}`);
      }
    ).finally(() => setCreating(false));
  };

  const revoke = (token: RunnerToken) => {
    api.revokeToken(token.id).then(
      () => { toast("Token revoked"); refresh(); },
      (reason: unknown) => {
        toast(reason instanceof ApiError && reason.isForbidden
          ? "You need the admin role for that"
          : `Could not revoke the token: ${reason instanceof Error ? reason.message : String(reason)}`);
      }
    );
  };

  return (
    <Card className="section-gap">
      <CardHeader
        title="Registration tokens"
        hint="one-time use"
        action={isAdmin ? <button type="button" className="btn sm primary" onClick={() => setForm(true)}>New token</button> : undefined}
      />

      {error && <div className="card-b"><ErrorBlock title="Registration tokens are unavailable." detail={error} onRetry={refresh} /></div>}

      {!error && (
        <TableWrap>
          <table>
            <thead>
              <tr><th>Token</th><th>Allowed labels</th><th>Expires</th><th>Used</th><th className="right">Actions</th></tr>
            </thead>
            <tbody>
              {(data ?? []).length === 0 ? (
                <tr><td colSpan={5}><Empty>No registration token is outstanding.</Empty></td></tr>
              ) : data!.map((token) => (
                <tr key={token.id}>
                  <td className="num">{token.id}</td>
                  <td>
                    {token.allowedLabels.length === 0
                      ? <span className="t2">any</span>
                      : token.allowedLabels.map((label) => <Pill key={label} variant="idle" dot={false}>{label}</Pill>)}
                  </td>
                  <td className="num t2">{absolute(token.expiresAt) ?? token.expiresAt}</td>
                  <td className="t2">{token.usedAt ? absolute(token.usedAt) : "—"}</td>
                  <td className="right">
                    {token.revokedAt ? <Pill variant="idle" dot={false}>revoked</Pill>
                      : token.usedAt ? <Pill variant="ok" dot={false}>consumed</Pill>
                        : isAdmin ? (
                          <button
                            type="button"
                            className="btn sm danger"
                            onClick={() => confirm({
                              title: "Revoke token",
                              body: "The token stops working immediately. Any runner that has not registered with it yet will need a new one.",
                              confirmLabel: "Revoke token",
                              danger: true,
                              onConfirm: () => revoke(token),
                            })}
                          >
                            Revoke
                          </button>
                        ) : <span className="t2">—</span>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </TableWrap>
      )}

      {form && (
        <Modal title="New registration token" onClose={() => setForm(false)}>
          <h2>New registration token</h2>
          <p>Leave the labels empty to let the token register a runner with any capabilities.</p>
          <div className="form-grid">
            <label className="wide">
              Allowed labels (comma separated)
              <input value={labels} placeholder="go, docker" onChange={(event) => setLabels(event.target.value)} />
            </label>
          </div>
          <div className="btnrow">
            <button type="button" className="btn primary" disabled={creating} onClick={create}>
              {creating ? "Creating…" : "Create token"}
            </button>
            <button type="button" className="btn" onClick={() => setForm(false)}>Cancel</button>
          </div>
        </Modal>
      )}

      {issued && <IssuedToken token={issued} onClose={() => setIssued(null)} />}
      {dialog}
    </Card>
  );
}

/** The token is shown once and never again — the server keeps only its hash. */
function IssuedToken({ token, onClose }: { token: CreatedToken; onClose: () => void }) {
  const toast = useToast();
  return (
    <Modal title="Runner registration token" onClose={onClose}>
      <h2>Runner registration token</h2>
      <p>Copy it now, it is shown only once. It registers exactly one runner and then stops working.</p>
      <div className="token-value mono">
        <span>{token.token}</span>
        <button
          type="button"
          className="btn sm"
          onClick={() => {
            navigator.clipboard?.writeText(token.token).then(
              () => toast("Token copied"),
              () => toast("Could not copy — select the token and copy it by hand")
            );
          }}
        >
          Copy
        </button>
      </div>
      <p className="t2">Expires {absolute(token.expiresAt) ?? token.expiresAt}.</p>
      <div className="btnrow">
        <button type="button" className="btn primary" onClick={onClose}>Done</button>
      </div>
    </Modal>
  );
}
