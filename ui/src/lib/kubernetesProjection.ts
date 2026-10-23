import type { KubernetesResource } from "@/lib/api";
import { isKnownKubernetesTime } from "./kubernetesTime";

const timestampKeys = new Set([
  "created_at",
  "creationTimestamp",
  "last_transition_at",
  "lastTransitionTime",
  "started_at",
  "startedAt",
  "last_terminated_at",
  "finished_at",
  "finishedAt",
  "firstTimestamp",
  "lastTimestamp",
  "first_timestamp",
  "last_timestamp",
  "observed_at",
]);

const sensitiveKeys = new Set([
  "config",
  "credential",
  "credentials",
  "data",
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
  if (typeof value === "number" || typeof value === "boolean" || value === null)
    return value;
  if (Array.isArray(value))
    return value.slice(0, 100).map((item) => clean(item, depth + 1));
  if (typeof value !== "object") return undefined;
  return Object.fromEntries(
    Object.entries(value)
      .filter(
        ([key, item]) =>
          item !== undefined &&
          (!timestampKeys.has(key) || isKnownKubernetesTime(item)),
      )
      .slice(0, 100)
      .map(([key, item]) => [
        key,
        key.toLowerCase() === "env"
          ? Array.isArray(item)
            ? item.slice(0, 100).map((entry: unknown) => {
                if (!entry || typeof entry !== "object" || !("name" in entry)) return "‹redacted›";
                return {
                  name: clean(entry.name, depth + 1),
                  ...("value" in entry ? { value: "‹redacted›" } : {}),
                  ...("valueFrom" in entry ? { valueFrom: clean(entry.valueFrom, depth + 1) } : {}),
                };
              })
            : "‹redacted›"
          : sensitiveKeys.has(key.toLowerCase())
          ? "‹redacted›"
          : clean(item, depth + 1),
      ]),
  );
}

function scalar(value: string | number | boolean | null): string {
  if (typeof value === "string") {
    const plain = /^(?:[A-Za-z_][A-Za-z0-9_.:/@+-]*|‹redacted›)$/.test(value)
      && !value.endsWith(":")
      && !/^(?:null|true|false|yes|no|on|off)$/i.test(value);
    return plain ? value : JSON.stringify(value);
  }
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
        return lines.length ? [`${prefix}- ${lines[0].trimStart()}`, ...lines.slice(1)] : [`${prefix}- {}`];
      }
      return [`${prefix}- ${scalar(item as string | number | boolean | null)}`];
    });
  }
  if (value !== null && typeof value === "object") {
    const entries = Object.entries(value);
    if (entries.length === 0) return [`${prefix}{}`];
    return entries.flatMap(([key, item]) => {
      const safeKey = /^[A-Za-z_][A-Za-z0-9_.-]*$/.test(key)
        ? key
        : JSON.stringify(key);
      if (item !== null && typeof item === "object") {
        return [`${prefix}${safeKey}:`, ...yamlLines(item, indent + 2)];
      }
      return [
        `${prefix}${safeKey}: ${scalar(item as string | number | boolean | null)}`,
      ];
    });
  }
  return [`${prefix}${scalar(value as string | number | boolean | null)}`];
}

export function projectedResourceYaml(resource: KubernetesResource): string {
  const summary = resource.summary ?? {};
  const { containers, node, selector, phase, container_states, log_container_states, pod_ip, qos_class, ...observations } = summary;
  const projectedContainers = Array.isArray(containers) ? containers.map((container: unknown) => {
    if (!container || typeof container !== "object") return container;
    const { requests, limits, resources, ...fields } = container as Record<string, unknown>;
    return { ...fields, ...(requests || limits || resources ? { resources: { ...(resources && typeof resources === "object" ? resources : {}), ...(requests ? { requests } : {}), ...(limits ? { limits } : {}) } } : {}) };
  }) : containers;
  const projection = clean({
    apiVersion: resource.api_version,
    kind: resource.kind,
    metadata: {
      name: resource.name,
      ...(resource.namespace ? { namespace: resource.namespace } : {}),
      ...(resource.uid ? { uid: resource.uid } : {}),
      ...(isKnownKubernetesTime(resource.created_at)
        ? { creationTimestamp: resource.created_at }
        : {}),
      ...(resource.owners?.length ? { ownerReferences: resource.owners } : {}),
    },
    ...(containers || node || selector
      ? {
          spec: {
            ...(node ? { nodeName: node } : {}),
            ...(projectedContainers ? { containers: projectedContainers } : {}),
            ...(selector ? { selector } : {}),
          },
        }
      : {}),
    ...(resource.conditions?.length || phase || container_states || log_container_states || pod_ip || qos_class
      ? {
          status: {
            ...(phase ? { phase } : {}),
            ...(pod_ip ? { podIP: pod_ip } : {}),
            ...(qos_class ? { qosClass: qos_class } : {}),
            ...(resource.conditions?.length
              ? { conditions: resource.conditions }
              : {}),
            ...(container_states || log_container_states
              ? { containerStatuses: container_states ?? log_container_states }
              : {}),
          },
        }
      : {}),
    ...(resource.kind === "Secret" ? { data: "‹redacted›" } : {}),
    ...(Object.keys(observations).length ? { observations } : {}),
  });
  return `${yamlLines(projection).join("\n")}\n`;
}

/** Locate the mapping delimiter without interpreting colons inside quoted keys or scalar URLs. */
export function yamlKeyEnd(line: string): number {
  let quote = "";
  for (let index = 0; index < line.length; index++) {
    const char = line[index];
    if (quote) {
      if (char === "\\" && quote === '"') index++;
      else if (char === quote) {
        if (quote === "'" && line[index + 1] === "'") index++;
        else quote = "";
      }
    } else if (char === '"' || char === "'") quote = char;
    else if (char === ":" && (index === line.length - 1 || /\s/.test(line[index + 1]))) return index;
  }
  return -1;
}
