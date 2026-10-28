// Section registries for the one-section-at-a-time settings layouts. Kept pure
// (no React) so URL resolution and legacy deep links are unit-testable.

export interface SettingsSectionDef {
  id: string;
  group: string;
  label: string;
  description: string;
  // enterprise marks a section whose control is an Enterprise capability.
  enterprise?: boolean;
  // readOnly marks a section that only displays YAML / environment config.
  readOnly?: boolean;
  docsUrl?: string;
}

const CONFIG_DOCS = "https://docs.versusincident.com/#/configuration/configuration";
const AGENT_CONFIG_DOCS = "https://docs.versusincident.com/#/agent/configuration";

export const ADMIN_SECTIONS: readonly SettingsSectionDef[] = [
  {
    id: "mode",
    group: "Runtime",
    label: "Runtime mode",
    description: "Choose whether the agent learns, observes, or alerts.",
    enterprise: true,
  },
  {
    id: "ai",
    group: "Runtime",
    label: "AI provider",
    description: "Model provider, endpoint, and API key used by the agent.",
    enterprise: true,
  },
  {
    id: "channels",
    group: "Notifications",
    label: "Channels",
    description: "Runtime credentials and delivery for each notification channel.",
    enterprise: true,
  },
  {
    id: "alert-fatigue",
    group: "Notifications",
    label: "Alert fatigue",
    description: "Route, correlate, and suppress repetitive alerts.",
    enterprise: true,
  },
  {
    id: "sso",
    group: "Access",
    label: "Identity providers",
    description: "Single sign-on providers and the login policy.",
    enterprise: true,
  },
  {
    id: "members",
    group: "Access",
    label: "Members & roles",
    description: "People who can sign in and what they can manage.",
    enterprise: true,
  },
];

// Old in-page anchors on /admin that external links and server-provided tool
// actions still target (e.g. /admin#agent-ai-settings).
export const ADMIN_LEGACY_HASHES: Readonly<Record<string, string>> = {
  "agent-ai-settings": "ai",
  "alert-fatigue-settings": "alert-fatigue",
};

export const SETTINGS_SECTIONS: readonly SettingsSectionDef[] = [
  {
    id: "server",
    group: "General",
    label: "Server",
    description: "Listener, public host, and incident storage.",
    readOnly: true,
    docsUrl: CONFIG_DOCS,
  },
  {
    id: "count-window",
    group: "General",
    label: "Incident counts",
    description: "How far back incident counts look across the app.",
  },
  {
    id: "service-health",
    group: "General",
    label: "Service health",
    description: "How often service health is evaluated and over what window.",
  },
  {
    id: "channels",
    group: "Alerting",
    label: "Alert channels",
    description: "Where incidents are delivered.",
    readOnly: true,
    docsUrl: CONFIG_DOCS,
  },
  {
    id: "queues",
    group: "Alerting",
    label: "Queue listeners",
    description: "Message queues that create incidents.",
    readOnly: true,
    docsUrl: CONFIG_DOCS,
  },
  {
    id: "oncall",
    group: "Alerting",
    label: "On-call",
    description: "Escalation provider and acknowledgement window.",
    readOnly: true,
    docsUrl: CONFIG_DOCS,
  },
  {
    id: "agent-runtime",
    group: "Agent",
    label: "Runtime",
    description: "Agent mode, polling, and redaction.",
    readOnly: true,
    docsUrl: AGENT_CONFIG_DOCS,
  },
  {
    id: "agent-sources",
    group: "Agent",
    label: "Data sources",
    description: "Log, metric, and trace sources the agent reads.",
    readOnly: true,
    docsUrl: AGENT_CONFIG_DOCS,
  },
  {
    id: "agent-ai",
    group: "Agent",
    label: "AI",
    description: "Model and limits configured for the agent.",
    readOnly: true,
    docsUrl: AGENT_CONFIG_DOCS,
  },
  {
    id: "agent-patterns",
    group: "Agent",
    label: "Pattern learning",
    description: "Catalog, miner, pre-filter rules, and service detection.",
    readOnly: true,
    docsUrl: AGENT_CONFIG_DOCS,
  },
  {
    id: "spike",
    group: "Agent",
    label: "Spike baseline",
    description: "The baseline volume spikes are scored against.",
  },
  {
    id: "reports",
    group: "Reports",
    label: "Incident report",
    description: "The shareable incident analytics report and its schedule.",
  },
];

// Pre-section ?tab= values on /settings, kept so existing links keep working.
export const SETTINGS_LEGACY_TABS: Readonly<Record<string, string>> = {
  alerting: "channels",
  agent: "agent-sources",
  tuning: "service-health",
};

export interface SectionLocation {
  section?: string | null;
  tab?: string | null;
  hash?: string | null;
}

export interface LegacyMaps {
  tabs?: Readonly<Record<string, string>>;
  hashes?: Readonly<Record<string, string>>;
}

export interface ResolvedSection {
  id: string;
  // fromLegacy is true when a legacy ?tab= or #hash selected the section, so
  // the caller should rewrite the URL to ?section=.
  fromLegacy: boolean;
}

// resolveSection picks the active section: a valid ?section= wins, then a
// legacy ?tab= or #hash, then the first section.
export function resolveSection(
  sections: readonly SettingsSectionDef[],
  location: SectionLocation,
  legacy: LegacyMaps = {},
): ResolvedSection {
  const known = (id: string | null | undefined) =>
    id != null && sections.some((s) => s.id === id);

  if (known(location.section)) {
    return { id: location.section as string, fromLegacy: false };
  }
  const fromTab = location.tab ? legacy.tabs?.[location.tab] : undefined;
  if (known(fromTab)) return { id: fromTab as string, fromLegacy: true };
  const hash = (location.hash ?? "").replace(/^#/, "");
  const fromHash = hash ? legacy.hashes?.[hash] : undefined;
  if (known(fromHash)) return { id: fromHash as string, fromLegacy: true };
  return { id: sections[0].id, fromLegacy: false };
}

// groupSections keeps registry order while bucketing sections by group.
export function groupSections(
  sections: readonly SettingsSectionDef[],
): Array<{ group: string; sections: SettingsSectionDef[] }> {
  const groups: Array<{ group: string; sections: SettingsSectionDef[] }> = [];
  for (const section of sections) {
    const existing = groups.find((g) => g.group === section.group);
    if (existing) existing.sections.push(section);
    else groups.push({ group: section.group, sections: [section] });
  }
  return groups;
}
