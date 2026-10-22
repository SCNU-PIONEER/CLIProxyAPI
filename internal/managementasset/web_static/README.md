This directory contains generated static assets for the optional `/plus/` dashboard.

From the repository root, run `npm ci --prefix web`, then
`npm run build:embed --prefix web` before compiling Go. The packaging script
replaces stale generated assets while preserving this README. Docker and the
integration workflow perform the same export-before-compile sequence.

Generated assets are ignored by Git. Without an exported `index.html`, Go still
builds and the Plus dashboard returns 404; the upstream control panel is unchanged.
