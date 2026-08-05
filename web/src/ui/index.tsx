// The console's component library (specification.md §2.5). Every component here
// exists in docs/design/web-console/mockup.html; the styling lives in
// src/styles.css so components carry no literal colours.
import { useRef, type ReactNode } from "react";
import { absolute, ageAgo } from "../format";
import { GATE_LABEL, statusMeta, type GateState, type PillVariant } from "../status";
import { useFocusTrap } from "./focus-trap";

// --- Pill -----------------------------------------------------------------

/**
 * Status chip. `dot` off turns it into a plain tag — labels and capabilities are
 * not states, and a status dot beside them reads as one.
 */
export function Pill({ variant, children, pulse = false, dot = true }: {
  variant: PillVariant;
  children: ReactNode;
  pulse?: boolean;
  dot?: boolean;
}) {
  return <span className={`pill p-${variant}${pulse ? " pulse" : ""}`}>{dot && <i />}{children}</span>;
}

export function StatusPill({ status }: { status: string }) {
  const meta = statusMeta(status);
  return <Pill variant={meta.variant} pulse={meta.pulse}>{meta.label}</Pill>;
}

// --- Card -----------------------------------------------------------------

export function Card({ className = "", children }: { className?: string; children: ReactNode }) {
  return <section className={`card ${className}`.trim()}>{children}</section>;
}

export function CardHeader({ title, hint, action }: { title: string; hint?: ReactNode; action?: ReactNode }) {
  return (
    <div className="card-h">
      <h2>{title}</h2>
      {hint !== undefined && <span className="hint">{hint}</span>}
      {action && <span className="card-action">{action}</span>}
    </div>
  );
}

// --- Stat tile ------------------------------------------------------------

export function StatTile({ label, value, suffix, note, noteBad = false }: {
  label: string;
  value: ReactNode;
  suffix?: string;
  note?: ReactNode;
  noteBad?: boolean;
}) {
  return (
    <div className="card tile">
      <span className="lab">{label}</span>
      <span className="val">{value}{suffix && <small> {suffix}</small>}</span>
      {note !== undefined && <span className={noteBad ? "note bad" : "note"}>{note}</span>}
    </div>
  );
}

// --- Meter ----------------------------------------------------------------

/** `used/budget` as a bar plus its counts; the bar turns crit at the cap. */
export function Meter({ used, budget, label }: { used: number; budget: number; label?: string }) {
  const spent = budget > 0 ? Math.min(100, (used / budget) * 100) : 0;
  return (
    <span className="meter" title={label ? `${label}: ${used} of ${budget}` : undefined}>
      <span className="bar"><span className={used >= budget ? "full" : undefined} style={{ width: `${spent}%` }} /></span>
      <span className="n">{used}/{budget}</span>
    </span>
  );
}

// --- Gates ----------------------------------------------------------------

export function GateRow({ label, state, pendingLabel }: { label: string; state: GateState; pendingLabel: string }) {
  const className = state === "not_reached" ? "unreached" : state;
  return (
    <div className="row-line">
      <span>{label}</span>
      <span className={`gate ${className}`}>{state === "pending" ? pendingLabel : GATE_LABEL[state]}</span>
    </div>
  );
}

// --- KV list --------------------------------------------------------------

export function KV({ children }: { children: ReactNode }) {
  return <dl className="kv">{children}</dl>;
}

export function KVRow({ label, children }: { label: string; children: ReactNode }) {
  return <><dt>{label}</dt><dd>{children}</dd></>;
}

// --- Relative time --------------------------------------------------------

export function Age({ timestamp, fallback = "never", suffix = true }: { timestamp?: string; fallback?: string; suffix?: boolean }) {
  const text = suffix ? ageAgo(timestamp, fallback) : (timestamp ? ageAgo(timestamp, fallback).replace(" ago", "") : fallback);
  return <span title={absolute(timestamp)}>{text}</span>;
}

// --- Table ----------------------------------------------------------------

export function TableWrap({ children }: { children: ReactNode }) {
  return <div className="tbl-wrap">{children}</div>;
}

/**
 * A table row that behaves like a link: clickable, focusable, activated by
 * Enter, and announced with its own label (specification.md §2.5).
 */
export function RowLink({ onOpen, label, children }: { onOpen: () => void; label: string; children: ReactNode }) {
  return (
    <tr
      className="rowlink"
      tabIndex={0}
      aria-label={label}
      onClick={onOpen}
      onKeyDown={(event) => {
        if (event.key === "Enter") {
          event.preventDefault();
          onOpen();
        }
      }}
    >
      {children}
    </tr>
  );
}

// --- Filter chips ---------------------------------------------------------

export function FilterChips<T extends string>({ options, value, onChange, label }: {
  options: ReadonlyArray<readonly [T, string]>;
  value: T;
  onChange: (next: T) => void;
  label: string;
}) {
  return (
    <div className="filters" role="group" aria-label={label}>
      {options.map(([key, text]) => (
        <button key={key} type="button" className="chip" aria-pressed={value === key} onClick={() => onChange(key)}>
          {text}
        </button>
      ))}
    </div>
  );
}

// --- Attention item, feed item -------------------------------------------

export function AttentionItem({ tone, title, detail, actions }: {
  tone: "wait" | "crit" | "warn";
  title: ReactNode;
  detail?: ReactNode;
  actions?: ReactNode;
}) {
  return (
    <div className="attn-item">
      <span className={`stripe ${tone}`} />
      <div className="tx">
        <div className="t">{title}</div>
        {detail !== undefined && <div className="d">{detail}</div>}
      </div>
      {actions && <div className="act">{actions}</div>}
    </div>
  );
}

export function FeedItem({ time, tone, children }: { time: string; tone: "thread" | "ok" | "wait" | "crit"; children: ReactNode }) {
  return (
    <div className="feed-item">
      <span className="tm">{time}</span>
      <span className={`ic ${tone}`} />
      <span className="tx">{children}</span>
    </div>
  );
}

// --- Banner ---------------------------------------------------------------

export function Banner({ lead, children, actions }: { lead: string; children: ReactNode; actions?: ReactNode }) {
  return (
    <div className="banner" role="alert">
      <span className="tx"><b>{lead}</b> {children}</span>
      {actions && <span className="act btnrow">{actions}</span>}
    </div>
  );
}

// --- Health strip ---------------------------------------------------------

export type ProbeTone = "ok" | "warn" | "crit" | "idle";

export function HealthStrip({ children, label }: { children: ReactNode; label: string }) {
  return <div className="health-strip" role="status" aria-label={label}>{children}</div>;
}

export function Probe({ tone, name, value, trailing = false }: { tone: ProbeTone; name?: string; value: ReactNode; trailing?: boolean }) {
  return (
    <span className={trailing ? "hp trailing" : "hp"}>
      {!trailing && <i className={tone} />}
      {name}
      <span className="v">{value}</span>
    </span>
  );
}

// --- States ---------------------------------------------------------------

export function Empty({ children }: { children: ReactNode }) {
  return <div className="empty">{children}</div>;
}

export function ErrorBlock({ title, detail, onRetry }: { title: string; detail?: string; onRetry?: () => void }) {
  return (
    <div className="error-block" role="alert">
      <p>{title}</p>
      {detail && <p>{detail}</p>}
      {onRetry && <button type="button" className="btn sm" onClick={onRetry}>Retry</button>}
    </div>
  );
}

/** Card-shaped placeholders: the view's layout, minus its data. */
export function Skeleton({ cards = 1 }: { cards?: number }) {
  return (
    <div className="grid" aria-hidden="true">
      {Array.from({ length: cards }, (_, index) => <div key={index} className="card skeleton skeleton-card" />)}
    </div>
  );
}

// --- Modal ------------------------------------------------------------

/** A modal dialog. Focus-trapped, Escape closes, backdrop click closes. */
export function Modal({ title, onClose, children }: { title: string; onClose: () => void; children: ReactNode }) {
  const panel = useRef<HTMLElement | null>(null);
  useFocusTrap(panel, onClose);

  return (
    <div className="modal-backdrop" role="presentation" onClick={(event) => { if (event.target === event.currentTarget) onClose(); }}>
      <section className="modal" role="dialog" aria-modal="true" aria-label={title} ref={panel}>
        {children}
      </section>
    </div>
  );
}

