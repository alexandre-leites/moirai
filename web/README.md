# Moirai Web Dashboard

Management dashboard for monitoring workflows, queue state, runner fleet operations, and projects. The dashboard consumes authenticated `/api/v1/events` SSE updates to refresh workflow, queue, and runner views.

## Setup

- Install dependencies: `npm install`
- Run development server: `npm run dev`

## Testing and building

- Run unit tests with coverage: `npm run test`
- Run type checks and linting: `npm run typecheck && npm run lint`
- Production build: `npm run build`
