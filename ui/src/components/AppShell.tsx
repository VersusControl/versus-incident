import { useCallback, useEffect, useRef, useState } from "react";
import { Outlet } from "react-router-dom";
import { X } from "lucide-react";
import { Sidebar, SidebarContent } from "./Sidebar";
import { ToastProvider } from "./Toast";
import { ShortcutOverlay } from "./ShortcutOverlay";
import { ShellContext } from "./shellContext";
import { ReauthModal } from "@/lib/auth";
import { useShortcuts } from "@/lib/hooks";

// AppShell: skip-link (first tab stop), desktop rail / mobile drawer,
// toast outlet, global shortcuts, re-auth modal mount.
export function AppShell() {
  const [drawer, setDrawer] = useState(false);
  const [help, setHelp] = useState(false);
  const mainRef = useRef<HTMLDivElement>(null);

  useShortcuts({ onHelp: useCallback(() => setHelp((h) => !h), []) });

  // Drawer: Escape closes; lock scroll while open.
  useEffect(() => {
    if (!drawer) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setDrawer(false);
    };
    document.addEventListener("keydown", onKey);
    const prev = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      document.removeEventListener("keydown", onKey);
      document.body.style.overflow = prev;
    };
  }, [drawer]);

  return (
    <ToastProvider>
      <ShellContext.Provider value={{ openDrawer: () => setDrawer(true) }}>
        <a
          href="#main"
          className="skip-link"
          onClick={(e) => {
            e.preventDefault();
            mainRef.current?.focus();
          }}
        >
          Skip to content
        </a>

        <div className="flex h-full overflow-hidden" data-testid="app-authenticated">
          <Sidebar />
          <div
            id="main"
            ref={mainRef}
            tabIndex={-1}
            className="flex min-w-0 flex-1 flex-col overflow-hidden outline-none"
          >
            <Outlet />
          </div>
        </div>

        {drawer && (
          <div className="fixed inset-0 z-overlay lg:hidden" role="dialog" aria-label="Navigation">
            <div className="absolute inset-0 bg-black/50" onClick={() => setDrawer(false)} />
            <div className="absolute bottom-0 left-0 top-0 w-64 motion-safe:animate-[peek-in-left_200ms_ease-out]">
              <button
                aria-label="Close navigation"
                className="absolute -right-10 top-3 rounded-control bg-ink-800 p-2 text-ink-200"
                onClick={() => setDrawer(false)}
              >
                <X size={16} />
              </button>
              <SidebarContent onNavigate={() => setDrawer(false)} />
            </div>
          </div>
        )}

        {help && <ShortcutOverlay onClose={() => setHelp(false)} />}
        <ReauthModal />
      </ShellContext.Provider>
    </ToastProvider>
  );
}
