import { useCallback, useEffect, useRef, useState } from "react";
import { Outlet, useLocation } from "react-router-dom";
import { X } from "lucide-react";
import { Sidebar, SidebarContent } from "./Sidebar";
import { ToastProvider } from "./Toast";
import { ShortcutOverlay } from "./ShortcutOverlay";
import { QueryErrorBoundary } from "./ErrorBoundary";
import { ShellContext } from "./shellContext";
import { ReauthModal } from "@/lib/auth";
import { useShortcuts } from "@/lib/hooks";
import { pathArea, useAccess } from "@/lib/access";
import { AccessDeniedPage } from "./AccessDenied";

// AppShell: skip-link (first tab stop), desktop rail / mobile drawer,
// toast outlet, global shortcuts, re-auth modal mount.
export function AppShell() {
  const [drawer, setDrawer] = useState(false);
  const [help, setHelp] = useState(false);
  const mainRef = useRef<HTMLDivElement>(null);
  const { pathname } = useLocation();
  // A page the caller's role cannot open is not mounted at all, so it sends
  // no request the server would refuse; a plain refusal stands in. Until the
  // session's permissions are known no page is mounted either, so a deep
  // link or reload sends nothing before the role is read.
  const access = useAccess();
  const allowed = pathname === "/" || access.can(pathArea(pathname));

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

        {/* overflow-clip (not hidden): a clipped frame can't be scrolled by focus or scrollIntoView. */}
        <div className="flex h-full min-h-0 overflow-clip" data-testid="app-authenticated">
          <Sidebar />
          <div
            id="main"
            ref={mainRef}
            tabIndex={-1}
            style={{ outline: "none" }}
            className="relative flex min-h-0 min-w-0 flex-1 flex-col overflow-clip outline-none focus-visible:outline-none"
          >
            {/* Keyed by pathname: a page crash keeps the shell navigable and
                navigating away remounts a clean boundary — and drops the cached
                payload the crashed page rendered from. */}
            <QueryErrorBoundary key={pathname} context="Couldn't render this page">
              {!access.ready ? (
                <div className="flex-1" aria-busy="true" data-shell-pending />
              ) : allowed ? (
                <Outlet />
              ) : (
                <AccessDeniedPage />
              )}
            </QueryErrorBoundary>
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
