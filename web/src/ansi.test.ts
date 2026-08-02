import { describe, expect, it } from "vitest";
import { ansiLines, parseAnsi, stripAnsi } from "./ansi";

const ESC = "\u001b";

// Taken verbatim from workflow 9d1dddb2's log events: opencode's tool lines open
// with a reset, and annotate the result in bright black.
const TOOL_LINE = `${ESC}[0m✱ ${ESC}[0mGrep "v0\\.2\\.[0-9]"${ESC}[90m 8 matches${ESC}[0m\n`;

describe("stripAnsi", () => {
  it("removes the sequences that made the timeline read as [0m litter", () => {
    expect(stripAnsi(TOOL_LINE)).toBe('✱ Grep "v0\\.2\\.[0-9]" 8 matches\n');
  });

  it("removes cursor moves, erases and window titles as well as colour", () => {
    expect(stripAnsi(`${ESC}[2K${ESC}[1Ahello${ESC}]0;a title${ESC}\\!`)).toBe("hello!");
  });

  it("leaves text that merely looks like an escape alone", () => {
    // No ESC byte: this is what a log says about a sequence, not the sequence.
    expect(stripAnsi("printf '\\033[0m' emits [0m")).toBe("printf '\\033[0m' emits [0m");
  });
});

describe("parseAnsi", () => {
  it("splits a real tool line into its plain and dimmed runs", () => {
    expect(parseAnsi(TOOL_LINE)).toEqual([
      { text: '✱ ', style: {} },
      { text: 'Grep "v0\\.2\\.[0-9]"', style: {} },
      { text: " 8 matches", style: { fg: "var(--ansi-8)" } },
      { text: "\n", style: {} },
    ]);
  });

  it("keeps a style open until it is closed, across other text", () => {
    const spans = parseAnsi(`${ESC}[31mred ${ESC}[1mand bold${ESC}[0m plain`);
    expect(spans).toEqual([
      { text: "red ", style: { fg: "var(--ansi-1)" } },
      { text: "and bold", style: { fg: "var(--ansi-1)", bold: true } },
      { text: " plain", style: {} },
    ]);
  });

  it("reads bright, 256-colour and truecolor selectors", () => {
    expect(parseAnsi(`${ESC}[92ma`)[0].style.fg).toBe("var(--ansi-10)");
    expect(parseAnsi(`${ESC}[38;5;9ma`)[0].style.fg).toBe("var(--ansi-9)");
    expect(parseAnsi(`${ESC}[38;5;196ma`)[0].style.fg).toBe("rgb(255 0 0)");
    expect(parseAnsi(`${ESC}[38;5;244ma`)[0].style.fg).toBe("rgb(128 128 128)");
    expect(parseAnsi(`${ESC}[38;2;12;34;56ma`)[0].style.fg).toBe("rgb(12 34 56)");
    expect(parseAnsi(`${ESC}[48;5;4ma`)[0].style.bg).toBe("var(--ansi-4)");
  });

  it("reads the colon-separated form the same way", () => {
    expect(parseAnsi(`${ESC}[38:2:12:34:56ma`)[0].style.fg).toBe("rgb(12 34 56)");
  });

  it("does not let an extended colour swallow the attribute after it", () => {
    // `38;5;1` consumes three parameters; the `1` that follows is bold, and a
    // parser that mis-counts would drop it or read it as another colour.
    expect(parseAnsi(`${ESC}[38;5;1;1ma`)[0].style).toEqual({ fg: "var(--ansi-1)", bold: true });
  });

  it("treats a bare ESC[m as a reset", () => {
    expect(parseAnsi(`${ESC}[1mbold${ESC}[ma`)[1].style).toEqual({});
  });

  it("clears only what each reset code names", () => {
    const spans = parseAnsi(`${ESC}[1;4;31mall${ESC}[24mno underline${ESC}[22mno bold`);
    expect(spans[0].style).toEqual({ bold: true, underline: true, fg: "var(--ansi-1)" });
    expect(spans[1].style).toEqual({ bold: true, underline: false, fg: "var(--ansi-1)" });
    expect(spans[2].style).toEqual({ bold: false, dim: false, underline: false, fg: "var(--ansi-1)" });
  });

  it("drops non-SGR sequences without disturbing the style around them", () => {
    const spans = parseAnsi(`${ESC}[31mred${ESC}[2Kstill red`);
    expect(spans.map((span) => span.text)).toEqual(["red", "still red"]);
    expect(spans[1].style.fg).toBe("var(--ansi-1)");
  });
});

describe("ansiLines", () => {
  it("groups runs into lines and carries style across the break", () => {
    expect(ansiLines(`${ESC}[31mone\ntwo`)).toEqual([
      [{ text: "one", style: { fg: "var(--ansi-1)" } }],
      [{ text: "two", style: { fg: "var(--ansi-1)" } }],
    ]);
  });

  it("shows only the last frame a carriage return left standing", () => {
    expect(ansiLines("10%\r50%\r100%")).toEqual([[{ text: "100%", style: {} }]]);
  });

  it("treats CRLF as a line break rather than an overwrite", () => {
    // Splitting on \n first leaves a trailing \r on the previous part; reading
    // that as an overwrite would silently delete every line of a CRLF log.
    expect(ansiLines("first\r\nsecond")).toEqual([
      [{ text: "first", style: {} }],
      [{ text: "second", style: {} }],
    ]);
  });

  it("keeps a blank line rather than collapsing it", () => {
    expect(ansiLines("a\n\nb").length).toBe(3);
  });
});
