import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  // The Plus UI is served as a static export under /plus/ by the Go backend.
  // basePath keeps every emitted asset and Next Link under that prefix, and
  // trailingSlash emits directory index.html files so the static file server
  // does not need rewrite rules.
  output: "export",
  basePath: "/plus",
  trailingSlash: true,
};

export default nextConfig;
