import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

const appSource = readFileSync(new URL("../App.tsx", import.meta.url), "utf8");
const sidebarSource = readFileSync(
  new URL("../components/Sidebar.tsx", import.meta.url),
  "utf8",
);

describe("chat integration policy", () => {
  it("keeps the cold chat page lazy and mounts the canonical route", () => {
    expect(appSource).toMatch(/lazyPage\(\(\) => import\("\.\/pages\/ChatPage"\)/);
    expect(appSource).toContain('<Route path="/agent/chat" element={<ChatPage />} />');
  });

  it("keeps Chat as the first AI navigation item", () => {
    const aiStart = sidebarSource.indexOf("const ai: SideItem[]");
    const chat = sidebarSource.indexOf('to: "/agent/chat"', aiStart);
    const decisions = sidebarSource.indexOf('to: "/agent/decisions"', aiStart);
    expect(chat).toBeGreaterThan(aiStart);
    expect(chat).toBeLessThan(decisions);
  });
});