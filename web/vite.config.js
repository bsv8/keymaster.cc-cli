import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

export default defineConfig(({ mode }) => ({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:2100',
        changeOrigin: false
      }
    }
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true
  },
  // Dev-only Origin allowance is handled by the Go daemon (development mode);
  // production builds never widen Host/Origin checks.
  define: {
    __DEV__: mode === 'development'
  }
}));
