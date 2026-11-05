// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ToastProvider } from "@/components/Toast";
import { AwsConnectPanel } from "./AwsConnectPanel";
import { AwsServiceAccessGrid } from "./AwsServiceAccessGrid";
import { api, type AwsConnectDraft } from "@/lib/api";
import {
  STAGING_ROLE_ARN,
  awsSettings,
  draft,
  fullServices,
  partialServices,
  regionOptions,
} from "./awsFixtures";

vi.mock("@/lib/api", async (importActual) => {
  const actual = await importActual<typeof import("@/lib/api")>();
  return {
    ...actual,
    api: {
      ...actual.api,
      createAwsDraft: vi.fn(),
      awsDraft: vi.fn(),
      verifyAwsDraft: vi.fn(),
      revealAwsExternalId: vi.fn(),
      discoverAwsRegions: vi.fn(),
      createAwsAccount: vi.fn(),
    },
  };
});

afterEach(cleanup);

beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(api.createAwsDraft).mockResolvedValue(draft());
  vi.mocked(api.awsDraft).mockResolvedValue(draft());
  vi.mocked(api.verifyAwsDraft).mockResolvedValue({ probe_id: "probe-1" });
  vi.mocked(api.discoverAwsRegions).mockResolvedValue(regionOptions());
  vi.mocked(api.createAwsAccount).mockResolvedValue(
    awsSettings().accounts[1],
  );
});

function renderPanel(
  props: Partial<React.ComponentProps<typeof AwsConnectPanel>> = {},
) {
  const onClose = vi.fn();
  const onCommitted = vi.fn();
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, retryDelay: 0 } },
  });
  const view = render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <AwsConnectPanel
          settings={awsSettings()}
          canWrite
          onClose={onClose}
          onCommitted={onCommitted}
          {...props}
        />
      </ToastProvider>
    </QueryClientProvider>,
  );
  return { ...view, onClose, onCommitted };
}

async function reachCheckAccess(roleArn = STAGING_ROLE_ARN) {
  const utils = renderPanel();
  fireEvent.click(await screen.findByRole("button", { name: "Continue" }));
  const arn = (await screen.findByLabelText("Role ARN")) as HTMLInputElement;
  fireEvent.change(arn, { target: { value: roleArn } });
  return utils;
}

describe("connect wizard — method step", () => {
  it("offers the quick-create link plus an offline download, with the principal to trust", async () => {
    renderPanel();
    expect(await screen.findByText("How do you want to connect?")).toBeTruthy();
    const create = screen.getByRole("link", {
      name: /Open CloudFormation quick-create/,
    }) as HTMLAnchorElement;
    expect(create.href).toContain("quickcreate");
    expect(create.href).toContain(
      encodeURIComponent("https://downloads.example.com/versus-reader.yaml"),
    );
    expect(screen.getByRole("link", { name: /Download template/ })).toBeTruthy();
    expect(
      (screen.getByLabelText("Versus principal ARN") as HTMLInputElement).value,
    ).toBe("arn:aws:iam::999900001111:role/versus-server");
  });

  it("offers the Terraform snippet in a copy field", async () => {
    renderPanel();
    fireEvent.click(await screen.findByRole("radio", { name: /Terraform/ }));
    const snippet = (await screen.findByLabelText("Terraform module")) as HTMLInputElement;
    expect(snippet.value).toContain('module "versus_reader"');
  });

  it("locks Organizations in the OSS build and stops Continue", async () => {
    renderPanel();
    fireEvent.click(await screen.findByRole("radio", { name: /AWS Organizations/ }));
    expect(await screen.findByTestId("enterprise-locked")).toBeTruthy();
    expect(
      (screen.getByRole("button", { name: "Continue" }) as HTMLButtonElement).disabled,
    ).toBe(true);
  });

  it("reaches check access from the keyboard alone", async () => {
    renderPanel();
    const cont = await screen.findByRole("button", { name: "Continue" });
    cont.focus();
    await userEvent.keyboard("{Enter}");
    expect(await screen.findByLabelText("Role ARN")).toBeTruthy();
  });
});

describe("connect wizard — check access", () => {
  it("fills the grid as probes land and never turns a throttle into a denial", async () => {
    let phase: "before" | "partial" | "full" = "before";
    vi.mocked(api.verifyAwsDraft).mockImplementation(async () => {
      phase = "partial";
      return { probe_id: "probe-1" };
    });
    vi.mocked(api.awsDraft).mockImplementation(async (): Promise<AwsConnectDraft> => {
      if (phase === "before") return draft({ assume: null, services: [] });
      if (phase === "partial") {
        phase = "full";
        return draft({ assume: { ok: true, latency_ms: 212 }, services: partialServices() });
      }
      return draft({ assume: { ok: true, latency_ms: 212 }, services: fullServices() });
    });

    await reachCheckAccess();
    fireEvent.click(screen.getByRole("button", { name: "Check access" }));

    expect(await screen.findByText("Role assumed in 212 ms")).toBeTruthy();
    expect(screen.getByLabelText("EC2: Readable")).toBeTruthy();
    expect(
      screen.getByLabelText("ECS: Not checked: AWS throttled, retrying"),
    ).toBeTruthy();
    // A throttle is not a denial and not a zero.
    expect(screen.queryByLabelText("ECS: Denied")).toBeNull();
    expect(
      (screen.getByRole("button", { name: "Continue to regions" }) as HTMLButtonElement)
        .disabled,
    ).toBe(true);

    await waitFor(
      () =>
        expect(
          (screen.getByRole("button", { name: "Continue to regions" }) as HTMLButtonElement)
            .disabled,
        ).toBe(false),
      { timeout: 4000 },
    );
    expect(screen.getByLabelText("ECS: Not checked: AWS throttled, retrying")).toBeTruthy();
    expect(document.body.textContent).toContain("20 readable");
    expect(document.body.textContent).toContain("1 denied");
  });

  it("keeps the denial local and copies the exact IAM fix", async () => {
    vi.mocked(api.awsDraft).mockResolvedValue(
      draft({ assume: { ok: true, latency_ms: 212 }, services: fullServices() }),
    );
    await reachCheckAccess();
    fireEvent.click(screen.getByRole("button", { name: "Check access" }));

    const copyFix = await screen.findByRole("button", {
      name: "Copy fix for ElastiCache",
    });
    const box = copyFix.closest("div");
    expect(box?.textContent).toContain("ElastiCache: Denied");
    expect(box?.textContent).toContain(
      "Add elasticache:Describe* to versus-reader",
    );
    // Everything else keeps its normal state.
    expect(screen.getByLabelText("EC2: Readable")).toBeTruthy();
    expect(screen.getByLabelText("Lambda: Readable")).toBeTruthy();
  });

  it("shows the mapped assume failure with its fix and keeps Continue closed", async () => {
    vi.mocked(api.awsDraft).mockResolvedValue(
      draft({
        assume: { ok: false, error_code: "external_id_mismatch" },
        services: [],
      }),
    );
    await reachCheckAccess();
    fireEvent.click(screen.getByRole("button", { name: "Check access" }));

    expect(
      await screen.findByText("Trust policy does not allow this ExternalId"),
    ).toBeTruthy();
    expect(screen.getByText(/Copy the ExternalId from this step/)).toBeTruthy();
    expect(
      (screen.getByRole("button", { name: "Continue to regions" }) as HTMLButtonElement)
        .disabled,
    ).toBe(true);
    // No raw AWS text anywhere.
    expect(document.body.textContent).not.toContain("AccessDenied");
    // Nothing claims to be in flight: every service is settled as not checked.
    expect(screen.queryByText("Checking…")).toBeNull();
    expect(screen.queryByText(/still checking/)).toBeNull();
    expect(
      screen.getByLabelText("ECS: Not checked: Role could not be assumed"),
    ).toBeTruthy();
    expect(screen.getAllByText("Role could not be assumed")).toHaveLength(24);
  });

  it("keeps the masked External ID suffix visible", async () => {
    vi.mocked(api.awsDraft).mockResolvedValue(
      draft({ assume: { ok: true, latency_ms: 212 }, services: fullServices() }),
    );
    await reachCheckAccess();
    const value = await screen.findByTestId("secret-value");
    expect(value.textContent).toContain("vrs-ext-");
    expect(value.textContent?.endsWith("7f3a")).toBe(true);
  });

  it("turns the verified ARN into a copy field", async () => {
    vi.mocked(api.awsDraft).mockResolvedValue(
      draft({ assume: { ok: true, latency_ms: 212 }, services: fullServices() }),
    );
    await reachCheckAccess();
    fireEvent.click(screen.getByRole("button", { name: "Check access" }));
    await screen.findByText("Role assumed in 212 ms");
    const arn = screen.getByLabelText("Role ARN") as HTMLInputElement;
    expect(arn.readOnly).toBe(true);
    expect(arn.value).toBe(STAGING_ROLE_ARN);
    expect(screen.getByRole("button", { name: "Copy Role ARN" })).toBeTruthy();
  });
});

describe("connect wizard — reconnect", () => {
  it("opens at check access with the stored ARN already verified", async () => {
    vi.mocked(api.awsDraft).mockResolvedValue(
      draft({
        method: "existing_role",
        role_arn: STAGING_ROLE_ARN,
        assume: { ok: true, latency_ms: 212 },
        services: fullServices(),
      }),
    );
    renderPanel({ mode: "reconnect", reconnectRoleArn: STAGING_ROLE_ARN });

    expect(await screen.findByText("Role assumed in 212 ms")).toBeTruthy();
    await waitFor(() =>
      expect(api.verifyAwsDraft).toHaveBeenCalledWith("draft-1", {
        role_arn: STAGING_ROLE_ARN,
      }),
    );
    const arn = screen.getByLabelText("Role ARN") as HTMLInputElement;
    expect(arn.value).toBe(STAGING_ROLE_ARN);
    expect(arn.readOnly).toBe(true);
    const steps = screen.getByRole("list", { name: "Connect account progress" });
    expect(
      within(steps).getByText("Check access").closest("li")?.getAttribute("aria-current"),
    ).toBe("step");
  });
});

describe("connect wizard — regions", () => {
  it("preselects regions with resources and refuses unenabled ones", async () => {
    vi.mocked(api.awsDraft).mockResolvedValue(
      draft({ assume: { ok: true, latency_ms: 212 }, services: fullServices() }),
    );
    await reachCheckAccess();
    fireEvent.click(screen.getByRole("button", { name: "Check access" }));
    await waitFor(
      () =>
        expect(
          (screen.getByRole("button", { name: "Continue to regions" }) as HTMLButtonElement)
            .disabled,
        ).toBe(false),
      { timeout: 4000 },
    );
    fireEvent.click(screen.getByRole("button", { name: "Continue to regions" }));

    const east = (await screen.findByLabelText(
      "US East (N. Virginia) (us-east-1)",
    )) as HTMLInputElement;
    const ireland = screen.getByLabelText(
      "Europe (Ireland) (eu-west-1)",
    ) as HTMLInputElement;
    const singapore = screen.getByLabelText(
      "Asia Pacific (Singapore) (ap-southeast-1)",
    ) as HTMLInputElement;
    expect(east.checked).toBe(true);
    expect(ireland.checked).toBe(true);
    expect(singapore.disabled).toBe(true);
    expect(singapore.checked).toBe(false);
    expect(screen.getByText("Not enabled")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Connect account" }));
    await waitFor(() => expect(api.createAwsAccount).toHaveBeenCalled());
    await waitFor(() =>
      expect(api.createAwsAccount).toHaveBeenCalledWith({
        draft_id: "draft-1",
        display_name: "AWS account 444455556666",
        regions: ["us-east-1", "eu-west-1"],
      }),
    );
  });
});

describe("service access grid", () => {
  it("renders 24 cells and calls unlanded probes Checking, never zero", () => {
    render(<AwsServiceAccessGrid services={partialServices()} columns={2} />);
    const list = screen.getByRole("list", { name: "Service access" });
    expect(list.querySelectorAll("li")).toHaveLength(24);
    expect(screen.getAllByText("Checking…")).toHaveLength(18);
    expect(screen.getByText(/18 checking/)).toBeTruthy();
    expect(screen.getByTestId("access-progress").textContent).toContain(
      "6 of 24 services checked",
    );
    expect(screen.queryByText("Denied")).toBeTruthy();
  });

  it("announces progress politely as results arrive", () => {
    render(<AwsServiceAccessGrid services={fullServices()} columns={2} />);
    const progress = screen.getByTestId("access-progress");
    expect(progress.getAttribute("role")).toBe("status");
    expect(progress.getAttribute("aria-live")).toBe("polite");
    expect(progress.textContent).toContain("24 of 24 services checked");
  });

  it("reads a throttled probe as Not checked, never as a denial", () => {
    render(<AwsServiceAccessGrid services={partialServices()} columns={2} />);
    const throttled = screen.getByLabelText(
      "ECS: Not checked: AWS throttled, retrying",
    );
    expect(within(throttled).getByText("Not checked")).toBeTruthy();
    expect(within(throttled).getByText(/AWS throttled, retrying/)).toBeTruthy();
  });

  it("wraps the reason under the name and never clips the service label", () => {
    render(<AwsServiceAccessGrid services={partialServices()} columns={2} />);
    const throttled = screen.getByLabelText(
      "ECS: Not checked: AWS throttled, retrying",
    );
    // The full three-letter badge/name is on screen, not a clipped initial.
    const name = within(throttled).getByText("ECS");
    expect(name.className).toContain("truncate");
    expect(name.getAttribute("title")).toBe("ECS");
    // The reason is its own line, free of any fixed height or truncation.
    const reason = within(throttled).getByText("AWS throttled, retrying");
    expect(reason.className).not.toContain("truncate");
    expect(reason.className).toContain("text-ink-300");
    expect(reason.parentElement?.className).toContain("flex-col");
  });

  it("stops every row when the role could not be assumed", () => {
    render(
      <AwsServiceAccessGrid services={[]} columns={2} assumeFailed />,
    );
    const list = screen.getByRole("list", { name: "Service access" });
    expect(screen.queryByText("Checking…")).toBeNull();
    expect(list.querySelectorAll("li")).toHaveLength(24);
    expect(
      screen.getAllByLabelText("ECS: Not checked: Role could not be assumed"),
    ).toHaveLength(1);
    expect(screen.getAllByText("Not checked")).toHaveLength(24);
    expect(screen.getAllByText("Role could not be assumed")).toHaveLength(24);
    // Nothing animates on a screen no probe will ever settle.
    expect(list.querySelectorAll(".animate-spin")).toHaveLength(0);
    expect(screen.getByTestId("access-progress").textContent).toContain(
      "Role could not be assumed",
    );
  });
});
