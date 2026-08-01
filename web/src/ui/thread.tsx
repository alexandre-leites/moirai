// The phase thread — the console's signature element (specification.md §2.6).
//
// A workflow drawn as a thread spun through its phases: gold where spun, dashed
// where still ahead, visibly cut where a blocked or failed run ended. The wobble
// on the spun segments is the point — it has to read as thread, not as a
// progress bar.
import type { Workflow } from "../api";
import { CUT_STATUSES, PHASES, PHASE_LABEL, reachedPhase, statusMeta } from "../status";

type ThreadProps = {
  workflow: Workflow;
  /** Mini variant: table rows and overview cards. No labels, no legend, no rings. */
  mini?: boolean;
  width?: number;
  height?: number;
};

export function PhaseThread({ workflow, mini = false, width, height }: ThreadProps) {
  const w = width ?? (mini ? 170 : 980);
  const h = height ?? (mini ? 12 : 56);
  const meta = statusMeta(workflow.status);
  const cut = CUT_STATUSES.has(workflow.status);
  const reached = reachedPhase(workflow);

  const x0 = mini ? 2 : 16;
  const x1 = w - (mini ? 2 : 16);
  const step = (x1 - x0) / (PHASES.length - 1);
  const y = mini ? h / 2 : Math.min(34, h - 26);
  const xs = PHASES.map((_, index) => x0 + step * index);
  const wobble = mini ? 1.2 : 2.2;

  const spun: string[] = [];
  const ahead: string[] = [];
  for (let i = 0; i < PHASES.length - 1; i += 1) {
    // The segment leaving the cut point is left blank: the thread ends there.
    if (cut && i === reached) continue;
    if (i < reached) {
      const lift = wobble * (i % 2 ? 1 : -1);
      spun.push(
        `M ${xs[i]} ${y} C ${xs[i] + step * 0.35} ${y - lift}, ${xs[i + 1] - step * 0.35} ${y + lift}, ${xs[i + 1]} ${y}`
      );
    } else {
      ahead.push(`M ${xs[i]} ${y} L ${xs[i + 1]} ${y}`);
    }
  }

  return (
    <svg
      className={mini ? "minithread" : "threadline"}
      viewBox={`0 0 ${w} ${h}`}
      width={mini ? w : "100%"}
      height={mini ? h : undefined}
      role="img"
      aria-label={`Workflow phase progress: ${meta.label}`}
    >
      <path d={ahead.join(" ")} fill="none" stroke="var(--border)" strokeWidth={mini ? 1.5 : 2} strokeDasharray="2 5" strokeLinecap="round" />
      <path d={spun.join(" ")} fill="none" stroke="var(--thread-mark)" strokeWidth={mini ? 2 : 2.5} strokeLinecap="round" />
      {cut && (
        <path
          d={`M ${xs[reached]} ${y} l ${step * 0.22} -4 M ${xs[reached]} ${y} l ${step * 0.26} 1.5 M ${xs[reached]} ${y} l ${step * 0.18} 5`}
          fill="none"
          stroke="var(--crit)"
          strokeWidth={mini ? 1.5 : 2}
          strokeLinecap="round"
        />
      )}
      {PHASES.map((phase, index) => {
        const x = xs[index];
        const isCut = cut && index === reached;
        const isCurrent = !cut && index === reached && workflow.status !== "completed";
        const isSpun = index < reached || workflow.status === "completed";

        if (mini) {
          if (isCut) return <circle key={phase} cx={x} cy={y} r={2.5} fill="var(--crit)" />;
          if (isCurrent) return <circle key={phase} cx={x} cy={y} r={3} fill="var(--thread-mark)" />;
          return null;
        }

        const emphasised = isCurrent || isCut;
        return (
          <g key={phase}>
            {isCurrent && (
              <>
                <circle cx={x} cy={y} r={8} fill="none" stroke="var(--thread-mark)" strokeWidth={1.5} opacity={0.5} />
                <circle className="spindle-core" cx={x} cy={y} r={4.5} fill="var(--thread-mark)" />
              </>
            )}
            {isCut && <circle cx={x} cy={y} r={4.5} fill="var(--crit)" />}
            {!isCurrent && !isCut && (
              isSpun
                ? <circle cx={x} cy={y} r={3.5} fill="var(--thread-mark)" />
                : <circle cx={x} cy={y} r={3} fill="var(--ground)" stroke="var(--border)" strokeWidth={1.5} />
            )}
            <text
              x={x}
              y={y + 24}
              textAnchor={index === 0 ? "start" : index === PHASES.length - 1 ? "end" : "middle"}
              fontSize="10.5"
              fontFamily="system-ui, sans-serif"
              fontWeight={emphasised ? 700 : 400}
              fill={emphasised ? (isCut ? "var(--crit)" : "var(--thread)") : "var(--ink-3)"}
            >
              {PHASE_LABEL[phase]}
            </text>
          </g>
        );
      })}
    </svg>
  );
}

export function ThreadLegend() {
  return (
    <div className="phase-legend">
      <span className="k"><span className="sw spun" />spun</span>
      <span className="k"><span className="sw ahead" />ahead</span>
      <span className="k"><span className="sw cut" />cut</span>
    </div>
  );
}
