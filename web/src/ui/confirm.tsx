import type { ReactNode } from "react";
import { Modal } from "./index";

export type Confirmation = {
  title: string;
  /** Says what will happen, in the operator's terms. Destructive actions must. */
  body: ReactNode;
  confirmLabel: string;
  danger?: boolean;
  onConfirm: () => void;
};

export function ConfirmDialog({ confirmation, onClose }: { confirmation: Confirmation; onClose: () => void }) {
  return (
    <Modal title={confirmation.title} onClose={onClose}>
      <h2>{confirmation.title}</h2>
      <p>{confirmation.body}</p>
      <div className="btnrow">
        <button
          type="button"
          className={confirmation.danger ? "btn danger" : "btn primary"}
          onClick={() => { onClose(); confirmation.onConfirm(); }}
        >
          {confirmation.confirmLabel}
        </button>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
      </div>
    </Modal>
  );
}
