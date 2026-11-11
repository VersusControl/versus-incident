// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { ToastProvider } from "@/components/Toast";
import { AwsSettingsPage } from "./AwsSettingsPage";
import { api } from "@/lib/api";
import {
  awsAccount,
  awsSettings,
  draft,
  stagingAccount,
} from "./awsFixtures";

vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      awsSettings: vi.fn(),
      createAwsDraft: vi.fn(),
      awsDraft: vi.fn(),
      verifyAwsDraft: vi.fn(),
      revealAwsExternalId: vi.fn(),
      discoverAwsRegions: vi.fn(),
      createAwsAccount: vi.fn(),
      recheckAwsAccount: vi.fn(),
      updateAwsAccount: vi.fn(),
      deleteAwsAccount: vi.fn(),
      updateAwsOptions: vi.fn(),
      updateAwsPrivacy: vi.fn(),
    },
  };
});

// The page is the subject; the shell header is exercised elsewhere.
vi.mock("@/components/TopBar", () => ({
  TopBar: ({ title, subtitle }: { title: string; subtitle?: string }) => (
    <h1>
      {title} {subtitle}
    </h1>
  ),
}));

const role = vi.hoisted(() => ({
  value: {} as Record<string, unknown>,
}));

vi.mock("@/lib/useEffectiveRole", () => ({
  useEffectiveRole: () => role.value,
}));

const ADMIN = {
  org: "acme",
  enterprise: true,
  role: "admin",
  isAdmin: true,
  hasSession: true,
  loading: false,
  session: {},
};

const VIEWER = { ...ADMIN, role: "viewer", isAdmin: false };

function setViewport(wide: boolean) {
  Object.defineProperty(window, "matchMedia", {
    value: vi.fn(() => ({
      matches: wide,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    })),
    configurable: true,
  });
}

afterEach(cleanup);

beforeEach(() => {
  vi.clearAllMocks();
  setViewport(true);
  role.value = ADMIN;
  vi.mocked(api.awsSettings).mockResolvedValue(awsSettings());
  vi.mocked(api.createAwsDraft).mockResolvedValue(draft());
  vi.mocked(api.awsDraft).mockResolvedValue(draft());
  vi.mocked(api.verifyAwsDraft).mockResolvedValue({ probe_id: "probe-1" });
  vi.mocked(api.discoverAwsRegions).mockResolvedValue([]);
  vi.mocked(api.createAwsAccount).mockResolvedValue(awsAccount());
  vi.mocked(api.recheckAwsAccount).mockResolvedValue(undefined);
  vi.mocked(api.updateAwsAccount).mockResolvedValue(awsAccount());
  vi.mocked(api.deleteAwsAccount).mockResolvedValue(undefined);
  vi.mocked(api.updateAwsOptions).mockResolvedValue(awsSettings());
  vi.mocked(api.updateAwsPrivacy).mockResolvedValue(awsSettings());
});

function renderPage() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, retryDelay: 0, staleTime: Infinity } },
  });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter>
          <AwsSettingsPage />
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

function answerLine(): HTMLElement {
  return document.querySelector("[data-answer-kind]") as HTMLElement;
}

describe("AWS settings page", () => {
  it("states the worst problem once and keeps the rest of the sentence plain", async () => {
    renderPage();
    await screen.findByText("Accounts");
    const answer = answerLine();
    expect(answer.getAttribute("data-answer-kind")).toBe("denied");
    expect(answer.textContent).toContain("3 accounts connected");
    expect(answer.textContent).toContain("Staging is missing ElastiCache access");
    expect(answer.textContent).toContain("cost data on");

    const coloured = Array.from(answer.querySelectorAll("span")).filter((span) =>
      span.className.includes("text-sev-critical"),
    );
    expect(coloured).toHaveLength(1);
    expect(coloured[0].textContent).toContain("ElastiCache");
    // The leading fact is not coloured, so the eye lands on one thing only.
    const leading = Array.from(answer.querySelectorAll("span")).find(
      (span) => span.textContent === "3 accounts connected",
    );
    expect(leading?.className).toContain("text-ink-100");
  });

  it("prints the account row facts with the denial only in its own row", async () => {
    renderPage();
    const staging = await screen.findByLabelText("Staging 444455556666");
    expect(within(staging).getByText("19 of 24 readable")).toBeTruthy();
    expect(within(staging).getByText("ElastiCache: Denied")).toBeTruthy();
    expect(within(staging).getByText(/^checked \d+ s ago$/)).toBeTruthy();
    expect(within(staging).getByText("2 of 3")).toBeTruthy();
    expect(within(staging).getByText("1 not enabled")).toBeTruthy();
    expect(within(staging).getByText("3 of 4 req/s")).toBeTruthy();

    const production = screen.getByLabelText("Production 111122223333");
    expect(within(production).queryByText(/Denied/)).toBeNull();
  });

  it("names a denied service from the array in the row and the answer line", async () => {
    vi.mocked(api.awsSettings).mockResolvedValue(
      awsSettings({
        accounts: [
          awsAccount(),
          stagingAccount({
            denied_services: ["rds"],
            not_on_plan_services: [],
          }),
        ],
      }),
    );
    renderPage();
    const staging = await screen.findByLabelText("Staging 444455556666");
    expect(within(staging).getByText("RDS: Denied")).toBeTruthy();
    expect(answerLine().textContent).toContain("Staging is missing RDS access");
  });

  it("summarises optional data in the accounts footer", async () => {
    renderPage();
    expect(
      await screen.findByText(
        "Optional data: Cost on in all accounts, Config in 2, Security Hub in 1",
      ),
    ).toBeTruthy();
  });

  it("shows what the AI sees, never sees, and the last leak check", async () => {
    renderPage();
    expect(await screen.findByText("Resource names, types and counts")).toBeTruthy();
    expect(screen.getByText("Alarm names and states")).toBeTruthy();
    expect(screen.getByText("Secrets, env vars and user data")).toBeTruthy();
    expect(screen.getByText("Log lines and request bodies")).toBeTruthy();
    expect(screen.getByLabelText("Tag value allowlist").textContent).toContain("cost-center");
    expect(screen.getByText("AI model: claude · sonnet-4")).toBeTruthy();
    expect(screen.getByText(/^Leak check: passing/)).toBeTruthy();
  });

  it("explains how to turn a disabled connector on instead of showing an empty page", async () => {
    vi.mocked(api.awsSettings).mockResolvedValue(
      awsSettings({
        enabled: false,
        accounts: [],
        enable_hint: {
          config_key: "connectors.aws.enable",
          env_var: "VERSUS_CONNECTORS_AWS_ENABLE",
        },
      }),
    );
    renderPage();
    expect(
      (await screen.findAllByText("AWS connector is off")).length,
    ).toBeGreaterThan(0);
    expect(
      (screen.getByLabelText("Config key") as HTMLInputElement).value,
    ).toBe("connectors.aws.enable: true");
    expect(
      (screen.getByLabelText("Environment variable") as HTMLInputElement).value,
    ).toBe("VERSUS_CONNECTORS_AWS_ENABLE=true");
    expect(screen.queryByRole("button", { name: "Add account" })).toBeNull();
  });

  it("opens the wizard at step one when no account is connected", async () => {
    vi.mocked(api.awsSettings).mockResolvedValue(awsSettings({ accounts: [] }));
    renderPage();
    expect(await screen.findByText("No AWS accounts connected")).toBeTruthy();
    expect(await screen.findByText("How do you want to connect?")).toBeTruthy();
    const cloudformation = screen.getByRole("radio", {
      name: /CloudFormation quick-create/,
    }) as HTMLInputElement;
    expect(cloudformation.checked).toBe(true);
    const step = screen.getByText("Method").closest("li");
    expect(step?.getAttribute("aria-current")).toBe("step");
  });

  it("stops at step one when the server has no AWS identity", async () => {
    vi.mocked(api.awsSettings).mockResolvedValue(
      awsSettings({ accounts: [], principal_arn: null }),
    );
    renderPage();
    expect(
      await screen.findByText("The Versus server has no AWS identity"),
    ).toBeTruthy();
    const cont = screen.getByRole("button", { name: "Continue" }) as HTMLButtonElement;
    expect(cont.disabled).toBe(true);
    expect(cont.title).toContain("AWS identity");
  });

  it("dates a disconnected account and offers reconnect without touching its neighbours", async () => {
    const disconnected = stagingAccount({
      status: "disconnected",
      checked_at: null,
      disconnected_since: new Date(Date.now() - 3600_000).toISOString(),
      error_code: "role_not_found",
    });
    vi.mocked(api.awsSettings).mockResolvedValue(
      awsSettings({ accounts: [awsAccount(), disconnected] }),
    );
    renderPage();
    expect(await screen.findByText(/^Staging disconnected since \d{2}:\d{2}$/)).toBeTruthy();
    expect(screen.getByRole("button", { name: "Reconnect" })).toBeTruthy();

    const production = screen.getByLabelText("Production 111122223333");
    expect(within(production).getByText("Connected")).toBeTruthy();
    expect(within(production).queryByRole("button", { name: "Reconnect" })).toBeNull();
  });

  it("renders every status as a word next to an icon, never colour alone", async () => {
    renderPage();
    const staging = await screen.findByLabelText("Staging 444455556666");
    const status = within(staging).getByText("Connected");
    expect(status.parentElement?.querySelector("svg")).toBeTruthy();
  });

  it("closes the connect panel with Escape and accepts a keyboard open", async () => {
    renderPage();
    const add = await screen.findByRole("button", { name: "Add account" });
    add.focus();
    await userEvent.keyboard("{Enter}");
    expect(await screen.findByText("How do you want to connect?")).toBeTruthy();

    fireEvent.keyDown(document, { key: "Escape" });
    await waitFor(() =>
      expect(screen.queryByText("How do you want to connect?")).toBeNull(),
    );
  });

  it("opens the wizard in a dialog below 1280 px and closes it with Escape", async () => {
    setViewport(false);
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Add account" }));

    const dialog = await screen.findByRole("dialog", {
      name: "Connect an AWS account",
    });
    expect(within(dialog).getByText("How do you want to connect?")).toBeTruthy();

    fireEvent.keyDown(document, { key: "Escape" });
    await waitFor(() =>
      expect(screen.queryByRole("dialog", { name: "Connect an AWS account" })).toBeNull(),
    );
  });

  it("gives a viewer the same facts with no write or reveal call", async () => {
    role.value = VIEWER;
    renderPage();

    expect(await screen.findByText("19 of 24 readable")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Add account" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Reconnect" })).toBeNull();
    expect(screen.queryByRole("button", { name: /Recheck/ })).toBeNull();
    expect(screen.getByText("Requires the admin role")).toBeTruthy();
    expect(
      (screen.getByLabelText("Requests per second") as HTMLInputElement).disabled,
    ).toBe(true);
    expect(
      (screen.getByLabelText("Read Cost data") as HTMLInputElement).disabled,
    ).toBe(true);

    expect(api.createAwsDraft).not.toHaveBeenCalled();
    expect(api.verifyAwsDraft).not.toHaveBeenCalled();
    expect(api.revealAwsExternalId).not.toHaveBeenCalled();
    expect(api.createAwsAccount).not.toHaveBeenCalled();
    expect(api.recheckAwsAccount).not.toHaveBeenCalled();
    expect(api.updateAwsOptions).not.toHaveBeenCalled();
    expect(api.updateAwsPrivacy).not.toHaveBeenCalled();
  });

  it("keeps config-defined values read-only for an admin", async () => {
    renderPage();
    await screen.findByText("Accounts");
    const budget = screen.getByLabelText("Requests per second") as HTMLInputElement;
    expect(budget.disabled).toBe(true);
    expect(budget.value).toBe("2");
    expect(screen.getByText("In use 1.5 of 2 req/s")).toBeTruthy();
    expect(screen.getAllByText("From config").length).toBeGreaterThan(0);
  });

  it("lets an admin edit a wizard-sourced budget and sends the new ceiling", async () => {
    vi.mocked(api.awsSettings).mockResolvedValue(
      awsSettings({
        budget: { used_rps: 1.5, budget_rps: 2, source: "wizard" },
      }),
    );
    renderPage();
    await screen.findByText("Accounts");
    const budget = screen.getByLabelText("Requests per second") as HTMLInputElement;
    expect(budget.disabled).toBe(false);
    fireEvent.change(budget, { target: { value: "3" } });
    fireEvent.click(screen.getByRole("button", { name: "Save budget" }));
    await waitFor(() =>
      expect(api.updateAwsOptions).toHaveBeenCalledWith({
        max_requests_per_second: 3,
      }),
    );
  });

  it("saves an optional-data switch inside the nested body", async () => {
    renderPage();
    await screen.findByText("Accounts");
    fireEvent.click(screen.getByLabelText("Read Trusted Advisor data"));
    await waitFor(() =>
      expect(api.updateAwsOptions).toHaveBeenCalledWith({
        optional_data: { trusted_advisor: true },
      }),
    );
  });

  it("treats a default-sourced switch as editable", async () => {
    vi.mocked(api.awsSettings).mockResolvedValue(
      awsSettings({
        optional_data: { trusted_advisor: { on: 0, source: "default" } },
      }),
    );
    renderPage();
    await screen.findByText("Accounts");
    const toggle = screen.getByLabelText(
      "Read Trusted Advisor data",
    ) as HTMLInputElement;
    expect(toggle.disabled).toBe(false);
    expect(toggle.checked).toBe(false);
    fireEvent.click(toggle);
    await waitFor(() =>
      expect(api.updateAwsOptions).toHaveBeenCalledWith({
        optional_data: { trusted_advisor: true },
      }),
    );
  });

  it("keeps a config-sourced switch disabled", async () => {
    vi.mocked(api.awsSettings).mockResolvedValue(
      awsSettings({
        optional_data: { trusted_advisor: { on: 1, source: "config" } },
      }),
    );
    renderPage();
    await screen.findByText("Accounts");
    const toggle = screen.getByLabelText(
      "Read Trusted Advisor data",
    ) as HTMLInputElement;
    expect(toggle.disabled).toBe(true);
    expect(toggle.checked).toBe(true);
  });

  it("keeps a wizard-sourced switch editable with its stored value", async () => {
    vi.mocked(api.awsSettings).mockResolvedValue(
      awsSettings({
        optional_data: { trusted_advisor: { on: 1, source: "wizard" } },
      }),
    );
    renderPage();
    await screen.findByText("Accounts");
    const toggle = screen.getByLabelText(
      "Read Trusted Advisor data",
    ) as HTMLInputElement;
    expect(toggle.disabled).toBe(false);
    expect(toggle.checked).toBe(true);
  });
});

describe("AWS settings failure states", () => {
  it("names a restricted role instead of dumping an HTTP error", async () => {
    const { ApiError } = await import("@/lib/api");
    vi.mocked(api.awsSettings).mockRejectedValue(new ApiError(403, "forbidden"));
    renderPage();
    expect(await screen.findByText("AWS settings are restricted")).toBeTruthy();
  });

  it("tells an older build apart from a network failure", async () => {
    const { ApiError } = await import("@/lib/api");
    vi.mocked(api.awsSettings).mockRejectedValue(new ApiError(404, "missing"));
    renderPage();
    expect(await screen.findByText("AWS settings are not available")).toBeTruthy();
  });
});

describe("AWS account removal", () => {
  async function removeFirstAccount() {
    renderPage();
    await screen.findByText("Accounts");
    fireEvent.click(screen.getAllByRole("button", { name: /^Remove / })[0]);
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Remove" }));
    expect(await screen.findByText("Account removed")).toBeTruthy();
  }

  it("adds the cost-data line when the purge is pending", async () => {
    vi.mocked(api.deleteAwsAccount).mockResolvedValue({ purge: "pending" });
    await removeFirstAccount();
    expect(screen.getByText("Cost data clears at the next refresh.")).toBeTruthy();
  });

  it("shows only the success title when no purge is pending", async () => {
    vi.mocked(api.deleteAwsAccount).mockResolvedValue({});
    await removeFirstAccount();
    expect(screen.queryByText("Cost data clears at the next refresh.")).toBeNull();
  });

  it("words a remove refused because storage is down, not as an AWS failure", async () => {
    const { ApiError } = await import("@/lib/api");
    vi.mocked(api.deleteAwsAccount).mockRejectedValue(
      new ApiError(503, "storage_unavailable", { error: "storage_unavailable" }),
    );
    renderPage();
    await screen.findByText("Accounts");
    fireEvent.click(screen.getAllByRole("button", { name: /^Remove / })[0]);
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Remove" }));
    expect(await screen.findByText("Storage is unavailable")).toBeTruthy();
    expect(screen.getByText("Try again shortly.")).toBeTruthy();
    expect(screen.queryByText("The AWS request failed")).toBeNull();
  });

  it("words a rename refused for a config-managed account, not as an AWS failure", async () => {
    const { ApiError } = await import("@/lib/api");
    vi.mocked(api.updateAwsAccount).mockRejectedValue(
      new ApiError(409, "config_managed", { error: "config_managed" }),
    );
    renderPage();
    await screen.findByText("Accounts");
    fireEvent.click(screen.getAllByRole("button", { name: /^Rename / })[0]);
    fireEvent.change(screen.getByLabelText("Account name"), { target: { value: "Renamed" } });
    fireEvent.click(screen.getByRole("button", { name: "Save name" }));
    expect(await screen.findByText("Set in the config file")).toBeTruthy();
    expect(screen.getByText("Change it in the config file.")).toBeTruthy();
    expect(screen.queryByText("The AWS request failed")).toBeNull();
  });

  it("words a save refused during a cost refresh, never the raw code", async () => {
    const { ApiError } = await import("@/lib/api");
    vi.mocked(api.updateAwsOptions).mockRejectedValue(
      new ApiError(409, "refresh_in_progress", { error: "refresh_in_progress" }),
    );
    renderPage();
    await screen.findByText("Accounts");
    fireEvent.click(screen.getByLabelText("Read Trusted Advisor data"));
    expect(await screen.findByText("A cost refresh is running")).toBeTruthy();
    expect(screen.getByText("Try again in a few seconds.")).toBeTruthy();
    expect(screen.queryByText("refresh_in_progress")).toBeNull();
  });
});

describe("AWS settings connect panel layout", () => {
  it("gives the right column the design width and keeps API use on one line", async () => {
    renderPage();
    const accounts = await screen.findByRole("region", {
      name: "Accounts",
    }).catch(async () => {
      const section = (await screen.findByText("Accounts")).closest("section");
      return section as HTMLElement;
    });
    fireEvent.click(screen.getByRole("button", { name: "Add account" }));

    // The two-column split matches the design: fluid left, 26-32rem right.
    const grid = accounts.parentElement?.parentElement as HTMLElement;
    expect(grid.className).toContain(
      "xl:grid-cols-[minmax(0,1fr)_minmax(26rem,32rem)]",
    );

    // "3 of 4 req/s" sits under the meter so it never wraps while the panel
    // takes its share of the width.
    const apiLabel = screen.getAllByText("3 of 4 req/s")[0];
    expect(apiLabel.className).toContain("whitespace-nowrap");
    expect(apiLabel.parentElement?.className).toContain("flex-col");
  });

  it("shows one heading in the mobile sheet and keeps the masked ID suffix", async () => {
    setViewport(false);
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Add account" }));

    const headings = await screen.findAllByRole("heading", {
      name: "Connect account",
    });
    expect(headings).toHaveLength(1);
    // The stepper still carries the three steps.
    expect(
      screen.getByRole("list", { name: "Connect account progress" }),
    ).toBeTruthy();

    // Step 2 renders the masked External ID with the design's last four.
    fireEvent.click(screen.getByRole("button", { name: "Continue" }));
    const arn = (await screen.findByLabelText("Role ARN")) as HTMLInputElement;
    fireEvent.change(arn, {
      target: { value: "arn:aws:iam::444455556666:role/versus-reader" },
    });
    expect((await screen.findByTestId("secret-value")).textContent).toContain(
      "7f3a",
    );
  });
});
