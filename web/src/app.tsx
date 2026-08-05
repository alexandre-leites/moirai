import { BrowserRouter, Route, Routes } from "react-router";
import type { ApiClient } from "./api";
import { AuthProvider } from "./auth";
import { LoginPage } from "./login";
import { Console } from "./shell";
import { ToastProvider } from "./ui/toast";

export function App({ api }: { api: ApiClient }) {
  return (
    <BrowserRouter>
      <AuthProvider api={api}>
        <ToastProvider>
          <Routes>
            <Route path="/login" element={<LoginPage />} />
            <Route path="*" element={<Console api={api} />} />
          </Routes>
        </ToastProvider>
      </AuthProvider>
    </BrowserRouter>
  );
}
