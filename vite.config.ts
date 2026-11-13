import { defineConfig } from "vite";

export default defineConfig({
  server: {
    // The app loads this address during `mygo dev` (devUrl in mygo.json).
    // NOT the vite default 5173: another project on this machine runs there.
    port: 5273,
    strictPort: true,
    // The development app and the packaged builds are not the page's.
    watch: { ignored: ["**/.mygo/**", "**/build/**"] },
  },
});
