// Mounting and querying helpers shared by the page tests.
//
// Deliberately not named `*.test.ts`: vitest's `include` would otherwise try to
// collect it as a suite of its own and fail it for having no tests. Every
// consumer opts into jsdom with a `// @vitest-environment jsdom` docblock, so
// the DOM globals used below are always present at call time.
import { act, type ReactElement } from "react";
import { createRoot, type Root } from "react-dom/client";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const mounted: Array<{ root: Root; container: HTMLElement }> = [];

/**
 * Unmounts everything `mount` created, so one test's effects (timers, pending
 * requests) cannot bleed into the next. Register it with `afterEach(unmountAll)`.
 */
export async function unmountAll(): Promise<void> {
  // One entry at a time, and each one is detached from the document even if its
  // unmount throws. Draining the whole array up front would strand every
  // remaining root — still mounted, still polling, and no longer reachable by a
  // later `afterEach` — which is the exact bleed this function exists to stop.
  while (mounted.length > 0) {
    const [entry] = mounted.splice(-1, 1);
    try {
      await act(async () => entry.root.unmount());
    } finally {
      entry.container.remove();
    }
  }
}

/** Mounts `element` into a fresh container and flushes its first effects. */
export async function mount(element: ReactElement): Promise<HTMLElement> {
  const container = document.createElement("div");
  document.body.appendChild(container);
  const root = createRoot(container);
  mounted.push({ root, container });
  await act(async () => {
    root.render(element);
  });
  return container;
}

/** Every button whose visible text matches `label`, in document order. */
export function buttons(container: ParentNode, label: RegExp): HTMLButtonElement[] {
  return Array.from(container.querySelectorAll("button")).filter((candidate) =>
    label.test(candidate.textContent ?? "")
  );
}

/**
 * The one button matching `label`. Throws when it is missing *or* ambiguous —
 * an assertion that silently picked the first of two would hide a real
 * regression in which control the operator is offered.
 */
export function button(container: ParentNode, label: RegExp): HTMLButtonElement {
  const found = buttons(container, label);
  if (found.length !== 1) {
    throw new Error(`expected exactly one button matching ${label}, found ${found.length}`);
  }
  return found[0];
}

/** Clicks an element and flushes the React work the click scheduled. */
export function click(element: HTMLElement): Promise<void> {
  return act(async () => {
    element.click();
  });
}

function labeled(container: ParentNode, label: RegExp): HTMLLabelElement {
  const found = Array.from(container.querySelectorAll("label")).filter((candidate) =>
    label.test(candidate.textContent ?? "")
  );
  if (found.length !== 1) {
    throw new Error(`expected exactly one label matching ${label}, found ${found.length}`);
  }
  return found[0];
}

/** The `<input>` inside the label whose text matches `label`. */
export function field(container: ParentNode, label: RegExp): HTMLInputElement {
  const input = labeled(container, label).querySelector("input");
  if (!input) throw new Error(`the label matching ${label} wraps no input`);
  return input;
}

/** The `<select>` inside the label whose text matches `label`. */
export function selectField(container: ParentNode, label: RegExp): HTMLSelectElement {
  const select = labeled(container, label).querySelector("select");
  if (!select) throw new Error(`the label matching ${label} wraps no select`);
  return select;
}

/** The single `<form>` in `container`. */
export function form(container: ParentNode): HTMLFormElement {
  const forms = Array.from(container.querySelectorAll("form"));
  if (forms.length !== 1) throw new Error(`expected exactly one form, found ${forms.length}`);
  return forms[0];
}

/**
 * Assigns through the prototype's own setter rather than `node.value = …`.
 * React installs a per-node value tracker whose setter swallows the change, so
 * a plain assignment updates the DOM but never reaches `onChange` and the
 * controlled component immediately paints the old value back.
 */
type ValueNode = HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement;

function setNativeValue(node: ValueNode, value: string): void {
  const prototype = node instanceof HTMLSelectElement
    ? HTMLSelectElement
    : node instanceof HTMLTextAreaElement
      ? HTMLTextAreaElement
      : HTMLInputElement;
  const setter = Object.getOwnPropertyDescriptor(prototype.prototype, "value")?.set;
  if (!setter) throw new Error(`${prototype.name}.prototype.value has no setter`);
  setter.call(node, value);
}

/** Types `value` into a controlled `<input>` or `<textarea>` as a keystroke would. */
export function typeInto(input: HTMLInputElement | HTMLTextAreaElement, value: string): Promise<void> {
  return act(async () => {
    setNativeValue(input, value);
    input.dispatchEvent(new Event("input", { bubbles: true }));
  });
}

/** The `<textarea>` labelled by `aria-label`. */
export function textarea(container: ParentNode, label: string): HTMLTextAreaElement {
  const found = container.querySelector<HTMLTextAreaElement>(`textarea[aria-label="${label}"]`);
  if (!found) throw new Error(`no textarea labelled "${label}"`);
  return found;
}

/** Picks `value` in a controlled `<select>`. */
export function chooseOption(select: HTMLSelectElement, value: string): Promise<void> {
  if (!Array.from(select.options).some((option) => option.value === value)) {
    throw new Error(`the select offers no option "${value}"`);
  }
  return act(async () => {
    setNativeValue(select, value);
    select.dispatchEvent(new Event("change", { bubbles: true }));
  });
}

/** Submits a form, as pressing Enter or its submit button would. */
export function submitForm(target: HTMLFormElement): Promise<void> {
  return act(async () => {
    target.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
  });
}

/** A promise plus the handles to settle it, for testing in-flight states. */
export function deferred<T>(): {
  promise: Promise<T>;
  resolve: (value: T) => Promise<void>;
  reject: (reason: unknown) => Promise<void>;
} {
  let settleWith: (value: T) => void = () => undefined;
  let failWith: (reason: unknown) => void = () => undefined;
  const promise = new Promise<T>((res, rej) => {
    settleWith = res;
    failWith = rej;
  });
  // Mark the original as handled. A test that rejects a deferred whose promise
  // nothing consumed would otherwise fail the run with an unhandled rejection
  // that vitest attributes to whichever test happened to be in flight — red CI
  // pointing at the wrong file. A real consumer's own handler is unaffected.
  promise.catch(() => undefined);
  return {
    promise,
    resolve: (value: T) =>
      act(async () => {
        settleWith(value);
      }),
    reject: (reason: unknown) =>
      act(async () => {
        failWith(reason);
      }),
  };
}
