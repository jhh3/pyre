import path from 'node:path';
import react from '@vitejs/plugin-react';
import tailwindcss from '@tailwindcss/vite';
import { defineConfig } from 'vite';

// The build is committed to internal/web/static/ and embedded by the Go
// server (go:embed), so Go builds and releases never need Node.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: { alias: { '@': path.resolve(import.meta.dirname, 'src') } },
  build: {
    outDir: path.resolve(import.meta.dirname, '../internal/web/static'),
    emptyOutDir: true,
    assetsDir: 'assets',
    sourcemap: false,
    // No inline <script>/<style> or data: URLs: the server's CSP is strict.
    assetsInlineLimit: 0,
    modulePreload: { polyfill: false },
    chunkSizeWarningLimit: 900,
  },
  server: {
    host: '127.0.0.1',
    proxy: { '/api': 'http://127.0.0.1:7390' },
  },
});
