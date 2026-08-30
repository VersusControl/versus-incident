// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import packageJson from "../../../package.json";
import { capChatMessage, safeMarkdownUrl } from "@/lib/markdownPolicy";
import { MarkdownText } from "./MarkdownText";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe("MarkdownText security policy", () => {
  it("drops images and does not request their URL", () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    const { container } = render(
      <MarkdownText>{"![secret](https://attacker.invalid/leak)"}</MarkdownText>,
    );
    expect(container.querySelector("img")).toBeNull();
    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it("escapes raw HTML and disables javascript and data links", () => {
    const { container } = render(
      <MarkdownText>
        {'<script>alert(1)</script> [bad](javascript:alert(1)) [data](data:text/html,bad)'}
      </MarkdownText>,
    );
    expect(container.querySelector("script")).toBeNull();
    expect(container.textContent).toContain("<script>alert(1)</script>");
    for (const link of container.querySelectorAll("a")) {
      expect(link.getAttribute("href")).toBeNull();
    }
  });

  it("hardens safe external and relative links", () => {
    const { container } = render(
      <MarkdownText>{"[docs](https://example.com) [local](/incidents/1)"}</MarkdownText>,
    );
    for (const link of container.querySelectorAll("a")) {
      expect(link.getAttribute("target")).toBe("_blank");
      expect(link.getAttribute("rel")).toBe("noopener noreferrer");
    }
    expect(container.querySelectorAll("a")[1].getAttribute("href")).toBe("/incidents/1");
  });

  it("rejects backslash, control, protocol-relative, and encoded separator bypasses", () => {
    for (const value of ["/\\attacker.invalid", "//attacker.invalid", "/%5cattacker.invalid", "/%2f%2fattacker.invalid", "/ok\u0000bad"]) {
      expect(safeMarkdownUrl(value)).toBe("");
    }
    expect(safeMarkdownUrl("/incidents/../services/api?q=1#trace")).toBe("/services/api?q=1#trace");
    expect(safeMarkdownUrl("https://example.com/docs")).toBe("https://example.com/docs");
    expect(safeMarkdownUrl("mailto:oncall@example.com")).toBe("mailto:oncall@example.com");
  });

  it("contains no raw-HTML dependency", () => {
    expect(packageJson.dependencies).not.toHaveProperty("rehype-raw");
    expect(packageJson.devDependencies).not.toHaveProperty("rehype-raw");
  });
});

describe("MarkdownText GFM", () => {
  it("renders tables, lists, fenced code, and copy control", () => {
    const { container } = render(
      <MarkdownText>
        {"| Signal | State |\n| --- | --- |\n| API | high |\n\n- one\n- two\n\n```json\n{\"ok\":true}\n```"}
      </MarkdownText>,
    );
    expect(container.querySelector("table")).not.toBeNull();
    expect(container.querySelectorAll("li")).toHaveLength(2);
    expect(screen.getByText("json")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Copy code" }));
  });

  it("caps by UTF-8 bytes without splitting surrogate pairs", () => {
    expect(capChatMessage("ab😀c", 6)).toBe("ab😀");
  });
});