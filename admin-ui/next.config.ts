import type { NextConfig } from 'next';
import path from 'node:path';

const adminApi = process.env.PULSYS_ADMIN_API ?? 'http://127.0.0.1:6060';
const staticExport = process.env.PULSYS_STATIC_EXPORT === '1';

const nextConfig: NextConfig = {
  outputFileTracingRoot: path.join(process.cwd()),
  ...(staticExport ? { output: 'export' as const } : {}),
  async rewrites() {
    if (staticExport) {
      return [];
    }
    return [
      { source: '/auth/:path*', destination: `${adminApi}/auth/:path*` },
      { source: '/admin/:path*', destination: `${adminApi}/admin/:path*` },
    ];
  },
};

export default nextConfig;
