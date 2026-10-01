import react from '@vitejs/plugin-react'
import { defineConfig } from 'vitest/config'

const backend = process.env.WEBPTY_DEV_BACKEND ?? 'http://127.0.0.1:8000'

// The embedded server serves the SPA at nested routes such as
// /admin/sessions/:id, so asset URLs must be absolute (/assets/...), not
// relative to the document.
export default defineConfig({
  base: '/',
  plugins: [react()],
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    assetsDir: 'assets',
    assetsInlineLimit: 0,
    sourcemap: false,
    rollupOptions: {
      output: {
        manualChunks: {
          react: ['react', 'react-dom', 'react-router'],
          xterm: ['@xterm/xterm', '@xterm/addon-fit'],
        },
      },
    },
  },
  server: {
    proxy: {
      '/api': { target: backend, ws: true },
      '/healthz': { target: backend },
    },
  },
  test: {
    environment: 'jsdom',
    setupFiles: './src/test/setup.ts',
    include: ['src/**/*.test.{ts,tsx}'],
    restoreMocks: true,
  },
})
