import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig(({ mode }) => ({
  plugins: [vue()],
  base: mode === 'web' ? '/' : './',
  publicDir: 'public',
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
  server: {
    port: 5173,
    proxy: mode === 'web'
      ? {
          '/admin': 'http://127.0.0.1:5642',
        }
      : undefined,
  },
}))
