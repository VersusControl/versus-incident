import { describe, expect, it } from "vitest";
import { projectedResourceYaml, yamlKeyEnd } from "./kubernetesProjection";

describe("Projected resource YAML presentation", () => {
  it("uses clean scalars and inline sequence mappings without duplicating the summary", () => {
    const yaml = projectedResourceYaml({ kind: "Pod", resource_id: "pods", name: "app", api_version: "v1", summary: {
      node: "node-a", phase: "Running", pod_ip: "10.0.0.1",
      containers: [{ name: "api", image: "registry:5000/app:v2", env: [{ name: "DB_PASSWORD", value: "never-show-this" }] }],
      safe_fact: "available",
    } });
    expect(yaml).toContain("apiVersion: v1");
    expect(yaml).toContain("- name: api");
    expect(yaml).toContain("image: registry:5000/app:v2");
    expect(yaml).toContain("- name: DB_PASSWORD\n          value: ‹redacted›");
    expect(yaml).not.toContain("never-show-this");
    expect(yaml).not.toContain("summary:");
    expect(yaml.match(/containers:/g)).toHaveLength(1);
    expect(yaml).toContain("safe_fact: available");
    expect(yaml).toContain("podIP:");
  });
  it("quotes ambiguous scalars and redacts non-array environment payloads", () => {
    const yaml = projectedResourceYaml({ kind: "Pod", resource_id: "pods", name: "true", summary: {
      containers: [{ name: "api", env: { DB_PASSWORD: "never-show-this" } }],
      values: { leaked: "never-show-this" }, safe: { flag: "yes", number: "0123", url: "https://example.test/a:b" },
    } });
    expect(yaml).toContain('name: "true"');
    expect(yaml).toContain('flag: "yes"');
    expect(yaml).toContain('number: "0123"');
    expect(yaml).toContain("url: https://example.test/a:b");
    expect(yaml).not.toContain("never-show-this");
  });
  it("quotes trailing mapping colons while preserving internal URL and image colons", () => {
    const yaml = projectedResourceYaml({ kind: "Pod", resource_id: "pods", name: "app", summary: {
      containers: [{ name: "api", image: "registry:5000/app:v2" }],
      ending_colon: "ended:", ending_url: "https://example.test/a:",
      url: "https://example.test/a:b", endings: ["one:", "two:"],
    } });
    expect(yaml).toContain('ending_colon: "ended:"');
    expect(yaml).toContain('ending_url: "https://example.test/a:"');
    expect(yaml).toContain('- "one:"');
    expect(yaml).toContain('- "two:"');
    expect(yaml).toContain("url: https://example.test/a:b");
    expect(yaml).toContain("image: registry:5000/app:v2");
  });
  it.each([
    ['  image: registry:5000/app:v2', 7],
    ['  url: https://example.test/a:b', 5],
    ['  "a:b": value', 7],
    ["  'a:b': value", 7],
    ['- https://example.test/a:b', -1],
    ['  "escaped\\":key": value', 17],
  ])("finds only the mapping colon in %s", (line, index) => {
    expect(yamlKeyEnd(line)).toBe(index);
  });
});
