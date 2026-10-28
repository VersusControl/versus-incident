const FOCUSABLE =
  "a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex='-1'])";

export function isTabbableCandidate(element: HTMLElement) {
  if (element.tabIndex < 0 || element.matches(":disabled") || element.closest("[disabled], [inert], [hidden], [aria-hidden='true']")) {
    return false;
  }
  const closedDetails = element.closest("details:not([open])");
  if (closedDetails) {
    const summary = Array.from(closedDetails.children).find((child) => child.tagName === "SUMMARY");
    if (!summary?.contains(element)) return false;
  }
  return true;
}

function isRendered(element: HTMLElement) {
  const style = window.getComputedStyle(element);
  return element.getClientRects().length > 0 && style.display !== "none" && style.visibility !== "hidden" && style.visibility !== "collapse";
}

export function tabbableCandidates(root: HTMLElement, isVisible: (element: HTMLElement) => boolean = isRendered) {
  return Array.from(root.querySelectorAll<HTMLElement>(FOCUSABLE))
    .filter((element) => isTabbableCandidate(element) && isVisible(element));
}