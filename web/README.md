# Moirai Web Dashboard

The web service is a React application built by Vite and served by nginx. Its nginx configuration serves the application on port `8080`, proxies `/api/` to the Compose `api:8080` service, and falls back to `index.html` for client-side routes.

## Development

```bash
npm ci
npm run dev
```

The Vite development server has no API proxy configuration. Use the Compose dashboard for `/api/` proxying, or configure a development reverse proxy outside this package.

## Build and validation

```bash
npm run typecheck
npm run lint
npm run build
```

There are no application-specific web environment variables in the current source tree.
