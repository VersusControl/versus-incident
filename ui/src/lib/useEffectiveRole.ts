import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { ApiError, getSsoSession, type SSOSession } from "@/lib/api";
import { useDeploymentOrg } from "@/lib/useDeploymentOrg";
import { isAdminRole } from "@/lib/role";

export interface EffectiveRoleState {
  // org is the resolved single-tenant deployment org, or null until resolved.
  org: string | null;
  // enterprise is true once the license-issued deployment org resolves — i.e.
  // the binary is licensed. It is false on a community / OSS binary (the
  // /deployment probe 403s/404s), which is how a control tells "render the
  // upsell" from "signed-in but read-only".
  enterprise: boolean;
  // role is the caller's effective RBAC role from the SSO whoami, or null when
  // there is no live session.
  role: string | null;
  // isAdmin is true when the role may manage enterprise config (admin/owner).
  isAdmin: boolean;
  // hasSession is true once the whoami resolves a live SSO session. It is the
  // signal that distinguishes an enterprise login (binary licensed, user signed
  // in) from a community binary / gateway-secret-only operator — so a control
  // can tell a logged-in viewer (show "requires admin") from "not enterprise".
  hasSession: boolean;
  // loading is true while the org or session is still resolving.
  loading: boolean;
  session: UseQueryResult<SSOSession, unknown>;
}

// useEffectiveRole resolves the caller's effective RBAC role from the SSO
// session whoami for the license-issued deployment org. It is the single
// source the admin controls gate on: `hasSession` separates an enterprise
// login from a community binary / gateway-secret operator, and `isAdmin`
// separates a manager from a read-only viewer. Any whoami ApiError (401 no
// session, 403 community) resolves to no session — fail closed.
export function useEffectiveRole(): EffectiveRoleState {
  const org = useDeploymentOrg();
  const orgId = org.data?.org ?? null;
  const session = useQuery<SSOSession>({
    queryKey: ["sso-session-role", orgId],
    queryFn: () => getSsoSession(orgId as string),
    enabled: Boolean(orgId),
    retry: (count, err) => {
      // A definite auth/license answer is terminal; only retry transient errors.
      if (err instanceof ApiError) return false;
      return count < 1;
    },
    staleTime: 30_000,
  });
  const role = session.data?.role ?? null;
  return {
    org: orgId,
    enterprise: orgId != null,
    role,
    isAdmin: isAdminRole(role),
    hasSession: session.isSuccess,
    loading: org.isPending || (Boolean(orgId) && session.isPending),
    session,
  };
}
