import { defineConfig } from 'vite';
import vue from '@vitejs/plugin-vue';

// Match the oldest candidate WKWebView syntax level; runtime APIs need their own checks.
export default defineConfig({ plugins: [vue()], clearScreen: false, build: { target: 'safari15' } });
