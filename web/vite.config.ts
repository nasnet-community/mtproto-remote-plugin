/// <reference types="vitest/config" />
import { defineConfig } from 'vite'
import { viteSingleFile } from 'vite-plugin-singlefile'
import { mockApi } from './mock-api'

// One self-contained index.html: the Go binary embeds it (internal/plugin/ui).
// The code is written against React's API and types, but runs on Preact: a
// fraction of the size, which the binary's size cap needs.
export default defineConfig({
  base: './',
  plugins: [viteSingleFile(), ...(process.env.MOCK ? [mockApi()] : [])],
  resolve: {
    alias: [
      { find: /^react-dom\/client$/, replacement: 'preact/compat/client' },
      { find: /^react\/jsx-runtime$/, replacement: 'preact/jsx-runtime' },
      { find: /^react\/jsx-dev-runtime$/, replacement: 'preact/jsx-dev-runtime' },
      { find: /^react(-dom)?$/, replacement: 'preact/compat' },
    ],
  },
  esbuild: { jsx: 'automatic', jsxImportSource: 'react' },
  server: {
    // `go run ./cmd/mtproto-remote` serves the API on :8080 while `npm run dev` serves the page.
    proxy: { '/api': 'http://localhost:8080' },
  },
  build: { target: 'es2020', chunkSizeWarningLimit: 1024 },
  test: { include: ['src/**/*.test.ts'] },
})
