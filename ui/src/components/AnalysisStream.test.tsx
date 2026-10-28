// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import type { AnalyzeEvent } from "@/lib/api";
import { AnalysisStream } from "./AnalysisStream";

afterEach(cleanup);

function toolEvent(overrides: Partial<AnalyzeEvent> = {}): AnalyzeEvent {
  return {
    seq: 1,
    at: "2026-08-26T00:00:00Z",
    kind: "tool_finished",
    tool: "query_metrics",
    ...overrides,
  };
}

describe("AnalysisStream tool display names", () => {
  it("describes a completed tool step as human investigation progress", () => {
    render(
      <AnalysisStream
        events={[toolEvent({ tool_display: "Checking metrics" })]}
        running={false}
        startedAt={Date.now()}
      />,
    );

    expect(
      screen.getByText(
        "I finished checking metrics and added what I found to the investigation.",
      ),
    ).toBeTruthy();
    expect(screen.queryByText("query_metrics")).toBeNull();
  });

  it("describes a running tool step as active first-person reasoning", () => {
    render(
      <AnalysisStream
        events={[
          toolEvent({
            kind: "tool_started",
            tool_display: "Checking metrics",
          }),
        ]}
        running
        startedAt={Date.now()}
      />,
    );

    expect(
      screen.getByText("I'm checking metrics to understand what happened…"),
    ).toBeTruthy();
  });

  it("explains a tool failure without sounding like a raw system event", () => {
    render(
      <AnalysisStream
        events={[
          toolEvent({
            kind: "tool_error",
            tool_display: "Checking metrics",
            error: "backend unavailable",
          }),
        ]}
        running
        startedAt={Date.now()}
      />,
    );

    expect(
      screen.getByText(
        "I couldn't finish checking metrics, so I'll continue with the evidence I have.",
      ),
    ).toBeTruthy();
  });

  it("falls back to a humanized sentence for older events", () => {
    render(
      <AnalysisStream
        events={[toolEvent()]}
        running={false}
        startedAt={Date.now()}
      />,
    );

    expect(
      screen.getByText(
        "I finished query metrics and added what I found to the investigation.",
      ),
    ).toBeTruthy();
  });
});