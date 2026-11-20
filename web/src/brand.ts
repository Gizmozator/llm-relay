// Brand name derived from the URL the dashboard is actually served on, so the
// logo and page title follow the deployment (and any future domain change)
// without hardcoding. Bare hosts only; dev hosts fall back to the project name.
export function brandName(): string {
  const host = window.location.hostname;
  if (!host || host === 'localhost' || /^\d+\.\d+\.\d+\.\d+$/.test(host)) return 'llm-relay';
  return host;
}
