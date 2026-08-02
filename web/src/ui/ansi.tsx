// Draws agent output the way the terminal it was written for would have. The
// parsing lives in ../ansi; this is only the mapping from a style to CSS.
import { Fragment, useMemo, type CSSProperties } from "react";
import { ansiLines, type AnsiSpan, type AnsiStyle } from "../ansi";

export function AnsiLog({ text, className }: { text: string; className?: string }) {
  const lines = useMemo(() => ansiLines(text), [text]);

  return (
    <pre className={className}>
      {lines.map((line, index) => (
        <Fragment key={index}>
          {index > 0 && "\n"}
          {line.map((span, position) => (
            <AnsiRun key={position} span={span} />
          ))}
        </Fragment>
      ))}
    </pre>
  );
}

function AnsiRun({ span }: { span: AnsiSpan }) {
  const style = spanStyle(span.style);
  // Most of a log carries no colour at all, and wrapping those runs would add a
  // DOM node per chunk to change nothing.
  if (style === null) return <>{span.text}</>;
  return <span style={style}>{span.text}</span>;
}

function spanStyle(style: AnsiStyle): CSSProperties | null {
  const decoration = [style.underline ? "underline" : "", style.strike ? "line-through" : ""]
    .filter(Boolean)
    .join(" ");

  // Inverse swaps the pair, falling back to the pane's own colours for whichever
  // side the sequence never set — which is the usual case, since `7m` is nearly
  // always used on its own to highlight a run.
  const foreground = style.inverse ? style.bg ?? "var(--ground)" : style.fg;
  const background = style.inverse ? style.fg ?? "var(--ink-2)" : style.bg;

  if (!foreground && !background && !style.bold && !style.dim && !style.italic && !decoration) {
    return null;
  }
  return {
    color: foreground,
    background,
    fontWeight: style.bold ? 600 : undefined,
    opacity: style.dim ? 0.7 : undefined,
    fontStyle: style.italic ? "italic" : undefined,
    textDecoration: decoration || undefined,
  };
}
