import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'
import { viteSingleFile } from 'vite-plugin-singlefile'
import { mockApi } from './mock-api'

// One self-contained index.html: the Rust binary embeds it (see build.rs).
export default defineConfig({
  base: './',
  plugins: [react(), viteSingleFile(), ...(process.env.MOCK ? [mockApi()] : [])],
  server: {
    // `cargo run` serves the API on :8080 while `npm run dev` serves the page.
    proxy: { '/api': 'http://localhost:8080' },
  },
  build: { target: 'es2020', chunkSizeWarningLimit: 1024 },
})
