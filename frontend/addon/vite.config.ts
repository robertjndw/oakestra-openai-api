import path from 'node:path';

import { oakestraAddon } from '@oakestra/dashboard-sdk/vite';
import tailwindcss from '@tailwindcss/vite';
import react from '@vitejs/plugin-react';
import { defineConfig } from 'vite';

// The SDK and @oakestra/ui are linked from a dashboard checkout next to this repository.
const dashboardPackages = path.resolve(import.meta.dirname, '../../../dashboard-v2/packages');

// An addons API that prefixes ids installs the addon under another name, so the build can be
// told which one. The container name, the Module Federation name and the API path all follow it.
const name = process.env.ADDON_NAME || 'openai-api';

export default defineConfig({
  plugins: [
    react(),
    tailwindcss(),
    // `prefix` must equal prefix(oai) in src/addon.css.
    oakestraAddon({ name, prefix: 'oai', port: 5182 }),
  ],
  server: {
    fs: { allow: [import.meta.dirname, dashboardPackages] },
    proxy: {
      // Same contract as the container's nginx: /api/v1/... is the backend's /admin/v1/....
      '/api': {
        target: process.env.OPENAI_API_DEV_API ?? 'http://127.0.0.1:8080',
        changeOrigin: true,
        rewrite: (p) => p.replace(/^\/api/, '/admin'),
      },
    },
  },
});
