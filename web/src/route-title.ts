// Per-route document titles, split out of shell.tsx so that file exports
// only components (react-refresh/only-export-components: routeTitle is a
// plain function, not a component).
const ROUTE_TITLES: Array<[RegExp, string]> = [
  [/^\/$/, "Overview"],
  [/^\/queue/, "Queue"],
  [/^\/workflows\/.+/, "Workflow"],
  [/^\/workflows/, "Workflows"],
  [/^\/runners/, "Runners"],
  [/^\/projects/, "Projects"],
  [/^\/account/, "Account"],
];

export function routeTitle(pathname: string): string {
  const match = ROUTE_TITLES.find(([pattern]) => pattern.test(pathname));
  return match ? `${match[1]} — Moirai Console` : "Moirai Console";
}
