import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

export default defineConfig({
  plugins: [react()],
  server: {
    // The connect-your-server page imports the deploy/ spoke templates with
    // ?raw; allow the repo root so `npm run dev` can read files above web/.
    fs: { allow: ['..'] },
    proxy: {
      '/api': 'http://127.0.0.1:8081',
      '/v1': 'http://127.0.0.1:8080',
    },
  },
});
