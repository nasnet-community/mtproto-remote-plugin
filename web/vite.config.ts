/// <reference types="vitest/config" />
import { defineConfig } from 'vite'
import { viteSingleFile } from 'vite-plugin-singlefile'
import { mockApi } from './mock-api'
import { mockLocal } from './mock-local'

// One self-contained index.html: the Go binary embeds it (internal/plugin/ui).
// The code is written against React's API and types, but runs on Preact: a
// fraction of the size, which the binary's size cap needs.
// PLUGIN=local builds Local MTProto's page instead (into dist-local), from the same code.
const LOCAL = process.env.PLUGIN === 'local'

export default defineConfig({
  base: './',
  define: { __LOCAL__: JSON.stringify(LOCAL) },
  plugins: [
    viteSingleFile(),
    { name: 'title', transformIndexHtml: (html) => (LOCAL ? html.replace('<title>Remote MTProto</title>', '<title>Local MTProto</title>') : html) },
    ...(process.env.MOCK ? [LOCAL ? mockLocal() : mockApi()] : []),
  ],
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
    // `go run ./cmd/mtproto-remote` (or mtproto-local) serves the API on :8080 while `npm run dev` serves the page.
    proxy: { '/api': 'http://localhost:8080' },
  },
  build: { target: 'es2020', chunkSizeWarningLimit: 1024, outDir: LOCAL ? 'dist-local' : 'dist' },
  test: { include: ['src/**/*.test.ts'] },
})
