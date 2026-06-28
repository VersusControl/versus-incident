import { createContext } from "react";

// ShellContext lets the page-rendered TopBar open the mobile nav drawer.
export const ShellContext = createContext<{ openDrawer: () => void } | null>(
  null,
);
