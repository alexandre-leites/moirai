// Agent output reaches the console as raw terminal bytes. The runner streams
// opencode's stdout verbatim (runner/internal/agents/opencode.go, streamedWriter)
// and opencode formats for a TTY, so every log event carries SGR escape
// sequences. Rendered as plain text they read as `[0m` litter — the ESC byte
// itself is unprintable — which is what the log panes showed before this module.
//
// Only presentation is reconstructed. Sequences that move the cursor, switch
// screen buffers, or set a window title mean nothing in a scrollback pane and
// are dropped rather than shown.

export type AnsiStyle = {
  fg?: string;
  bg?: string;
  bold?: boolean;
  dim?: boolean;
  italic?: boolean;
  underline?: boolean;
  strike?: boolean;
  inverse?: boolean;
};

/** A run of text that shares one style. */
export type AnsiSpan = {
  text: string;
  style: AnsiStyle;
};

// CSI is `ESC [`, parameter bytes (0x30-0x3F), intermediate bytes (0x20-0x2F),
// then one final byte (0x40-0x7E) naming the operation — `m` is the only one
// that carries presentation. OSC is `ESC ]` up to BEL or ST. The last branch
// covers the two-byte escapes, which have no parameters at all.
const ESCAPE_PATTERN = String.raw`\x1b(?:\[([0-?]*)[ -/]*([@-~])|\][\s\S]*?(?:\x07|\x1b\\)|[@-Z\\_])`;

// Built per call: a shared /g regex carries lastIndex between callers, which
// would make parseAnsi's result depend on who ran before it.
function escapes(): RegExp {
  return new RegExp(ESCAPE_PATTERN, "g");
}

/** The text with every escape sequence removed, for single-line contexts. */
export function stripAnsi(input: string): string {
  return input.replace(escapes(), "");
}

/** Splits the text into styled runs, tracking SGR state across the whole input. */
export function parseAnsi(input: string): AnsiSpan[] {
  const spans: AnsiSpan[] = [];
  const pattern = escapes();
  let style: AnsiStyle = {};
  let cursor = 0;
  let match: RegExpExecArray | null;

  while ((match = pattern.exec(input)) !== null) {
    if (match.index > cursor) spans.push({ text: input.slice(cursor, match.index), style });
    cursor = match.index + match[0].length;
    if (match[2] === "m") style = applySelectGraphic(style, match[1] ?? "");
  }
  if (cursor < input.length) spans.push({ text: input.slice(cursor), style });
  return spans;
}

/**
 * The same runs, grouped into the lines a terminal would have drawn.
 *
 * Style carries across a line break, because a sequence that opens a colour and
 * closes it several lines later is ordinary agent output.
 */
export function ansiLines(input: string): AnsiSpan[][] {
  // CRLF is one line break; only a bare CR rewrites the line in place.
  const lines: AnsiSpan[][] = [[]];

  for (const span of parseAnsi(input.replace(/\r\n/g, "\n"))) {
    const parts = span.text.split("\n");
    for (let index = 0; index < parts.length; index += 1) {
      if (index > 0) lines.push([]);
      let text = parts[index];
      const rewrite = text.lastIndexOf("\r");
      if (rewrite >= 0) {
        // Everything before the last carriage return was overwritten where it
        // stood. Progress bars and spinners depend on this; concatenating their
        // frames instead would bury the log under its own animation.
        lines[lines.length - 1] = [];
        text = text.slice(rewrite + 1);
      }
      if (text !== "") lines[lines.length - 1].push({ text, style: span.style });
    }
  }
  return lines;
}

function applySelectGraphic(current: AnsiStyle, parameters: string): AnsiStyle {
  // `ESC[m` means `ESC[0m`: an omitted parameter is zero. Sub-parameters are
  // written with colons by terminals that follow ECMA-48 strictly and with
  // semicolons by everything else, so both separators are read the same way.
  const codes = (parameters === "" ? "0" : parameters)
    .replace(/:/g, ";")
    .split(";")
    .map((part) => (part === "" ? 0 : Number(part)));

  let style: AnsiStyle = { ...current };
  for (let index = 0; index < codes.length; index += 1) {
    const code = codes[index];
    if (!Number.isFinite(code)) continue;

    switch (code) {
      case 0: style = {}; break;
      case 1: style = { ...style, bold: true }; break;
      case 2: style = { ...style, dim: true }; break;
      case 3: style = { ...style, italic: true }; break;
      case 4: style = { ...style, underline: true }; break;
      case 7: style = { ...style, inverse: true }; break;
      case 9: style = { ...style, strike: true }; break;
      // 21 is "doubly underlined" on some terminals and "bold off" on others.
      // Every implementation agrees it ends bold, which is all that is modelled.
      case 21:
      case 22: style = { ...style, bold: false, dim: false }; break;
      case 23: style = { ...style, italic: false }; break;
      case 24: style = { ...style, underline: false }; break;
      case 27: style = { ...style, inverse: false }; break;
      case 29: style = { ...style, strike: false }; break;
      case 39: style = { ...style, fg: undefined }; break;
      case 49: style = { ...style, bg: undefined }; break;
      case 38:
      case 48: {
        const extended = extendedColour(codes, index);
        index = extended.consumed;
        if (extended.colour === undefined) break;
        style = code === 38 ? { ...style, fg: extended.colour } : { ...style, bg: extended.colour };
        break;
      }
      default: {
        const named = namedColour(code);
        if (!named) break;
        style = named.slot === "fg" ? { ...style, fg: named.value } : { ...style, bg: named.value };
        break;
      }
    }
  }
  return style;
}

function namedColour(code: number): { slot: "fg" | "bg"; value: string } | null {
  if (code >= 30 && code <= 37) return { slot: "fg", value: paletteColour(code - 30) };
  if (code >= 40 && code <= 47) return { slot: "bg", value: paletteColour(code - 40) };
  if (code >= 90 && code <= 97) return { slot: "fg", value: paletteColour(code - 90 + 8) };
  if (code >= 100 && code <= 107) return { slot: "bg", value: paletteColour(code - 100 + 8) };
  return null;
}

/**
 * `38`/`48` take their colour from the parameters that follow: `5;n` selects
 * the 256-colour table, `2;r;g;b` gives the channels directly. Returns the
 * index of the last parameter consumed, so the caller resumes after it.
 */
function extendedColour(codes: number[], index: number): { colour?: string; consumed: number } {
  const selector = codes[index + 1];
  if (selector === 5) {
    return { colour: indexedColour(codes[index + 2]), consumed: index + 2 };
  }
  if (selector === 2) {
    const channels = [codes[index + 2], codes[index + 3], codes[index + 4]];
    if (!channels.every((value) => Number.isInteger(value) && value >= 0 && value <= 255)) {
      return { consumed: index + 4 };
    }
    return { colour: `rgb(${channels[0]} ${channels[1]} ${channels[2]})`, consumed: index + 4 };
  }
  // An unrecognised selector: skip it alone rather than guessing how many
  // parameters it would have carried and swallowing an unrelated attribute.
  return { consumed: index + 1 };
}

function indexedColour(value: number): string | undefined {
  if (!Number.isInteger(value) || value < 0 || value > 255) return undefined;
  if (value < 16) return paletteColour(value);
  if (value < 232) {
    // The 6×6×6 cube. Level 0 is black and the rest start at 95, stepping by 40.
    const offset = value - 16;
    const channel = (level: number) => (level === 0 ? 0 : 55 + level * 40);
    const red = channel(Math.floor(offset / 36) % 6);
    const green = channel(Math.floor(offset / 6) % 6);
    const blue = channel(offset % 6);
    return `rgb(${red} ${green} ${blue})`;
  }
  const grey = 8 + (value - 232) * 10;
  return `rgb(${grey} ${grey} ${grey})`;
}

// The sixteen named colours are tokens rather than literals so both themes stay
// honest — the console's log pane is parchment in light mode, not a black
// terminal, and a stock palette is unreadable on it (styles.css, --ansi-*).
function paletteColour(index: number): string {
  return `var(--ansi-${index})`;
}
