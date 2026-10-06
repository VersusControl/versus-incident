import type { KubernetesResource } from "@/lib/api";

const sensitiveKeys = new Set([
  "config",
  "credential",
  "credentials",
  "data",
  "env",
  "hooks",
  "manifest",
  "password",
  "privatekey",
  "secret",
  "stringdata",
  "token",
  "value",
  "values",
]);

function clean(value: unknown, depth = 0): unknown {
  if (depth > 8) return "[truncated]";
  if (typeof value === "string") return value.slice(0, 2048);
  if (typeof value === "number" || typeof value === "boolean" || value === null) return value;
  if (Array.isArray(value)) return value.slice(0, 100).map((item) => clean(item, depth + 1));
  if (typeof value !== "object") return undefined;
  return Object.fromEntries(
    Object.entries(value)
      .filter(([key, item]) => item !== undefined && !sensitiveKeys.has(key.toLowerCase()))
      .slice(0, 100)
      .map(([key, item]) => [key, clean(item, depth + 1)]),
  );
}

function scalar(value: string | number | boolean | null): string {
  if (typeof value === "string") return JSON.stringify(value);
  if (value === null) return "null";
  return String(value);
}

function yamlLines(value: unknown, indent = 0): string[] {
  const prefix = " ".repeat(indent);
  if (Array.isArray(value)) {
    if (value.length === 0) return [`${prefix}[]`];
    return value.flatMap((item) => {
      if (item !== null && typeof item === "object") {
        const lines = yamlLines(item, indent + 2);
        return [`${prefix}-`, ...lines];
      }
      return [`${prefix}- ${scalar(item as string | number | boolean | null)}`];
    });
  }
  if (value !== null && typeof value === "object") {
    const entries = Object.entries(value);
    if (entries.length === 0) return [`${prefix}{}`];
    return entries.flatMap(([key, item]) => {
      const safeKey = /^[A-Za-z_][A-Za-z0-9_.-]*$/.test(key) ? key : JSON.stringify(key);
      if (item !== null && typeof item === "object") {
        return [`${prefix}${safeKey}:`, ...yamlLines(item, indent + 2)];
      }
      return [`${prefix}${safeKey}: ${scalar(item as string | number | boolean | null)}`];
    });
  }
  return [`${prefix}${scalar(value as string | number | boolean | null)}`];
}

export function projectedResourceYaml(resource: KubernetesResource): string {
  const projection = clean({
    apiVersion: resource.api_version,
    kind: resource.kind,
    metadata: {
      name: resource.name,
      ...(resource.namespace ? { namespace: resource.namespace } : {}),
      ...(resource.uid ? { uid: resource.uid } : {}),
    },
    ...(resource.conditions?.length ? { conditions: resource.conditions } : {}),
    ...(resource.summary ? { summary: resource.summary } : {}),
  });
  return `${yamlLines(projection).join("\n")}\n`;
}
