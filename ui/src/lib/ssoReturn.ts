export function ssoLoginWithReturn(loginUrl: string, location: Pick<Location, "pathname" | "search">) {
  const relative = `${location.pathname}${location.search}`;
  const returnTo = relative.startsWith("/") && !relative.startsWith("//") && !relative.includes("\\")
    ? relative : "/";
  const url = new URL(loginUrl, window.location.origin);
  url.searchParams.set("return_to", returnTo);
  return `${url.pathname}${url.search}${url.hash}`;
}
