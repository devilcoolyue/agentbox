// abox-link embeds its own assets. Keep the shared, dependency-free core exact.
import { copyFile } from 'node:fs/promises';
await copyFile(new URL('../internal/web/static/js/i18n/core.js', import.meta.url),
  new URL('../internal/linkapp/static/i18n-core.js', import.meta.url));
