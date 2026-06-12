import { useCallback, useState } from "react";

// Theme switching — a pure CSS-variable swap (see index.css). Dark is the
// default (the 3am ops context); the explicit choice persists across
// sessions. The sidebar/drawer stay dark in both themes via .force-dark.
const KEY = "versus.theme";

export type Theme = "dark" | "light";

export function getStoredTheme(): Theme {
  try {
    return localStorage.getItem(KEY) === "light" ? "light" : "dark";
  } catch {
    return "dark";
  }
}

export function applyTheme(theme: Theme) {
  if (theme === "light") {
    document.documentElement.setAttribute("data-theme", "light");
  } else {
    document.documentElement.removeAttribute("data-theme");
  }
}

export function useTheme() {
  const [theme, setTheme] = useState<Theme>(getStoredTheme);
  const toggle = useCallback(() => {
    setTheme((cur) => {
      const next: Theme = cur === "dark" ? "light" : "dark";
      try {
        localStorage.setItem(KEY, next);
      } catch {
        /* private mode — theme just won't persist */
      }
      applyTheme(next);
      return next;
    });
  }, []);
  return { theme, toggle };
}
