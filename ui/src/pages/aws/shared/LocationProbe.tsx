import { useLocation } from "react-router-dom";

/** Shows where the router is, for tests that follow a navigation. */
export function LocationProbe() {
  const location = useLocation();
  return <output data-testid="location">{`${location.pathname}${location.search}`}</output>;
}
