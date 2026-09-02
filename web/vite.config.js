import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { fileURLToPath, URL } from "node:url";

/**
 * 开发代理目标允许通过环境变量覆盖，便于在不修改源码的情况下连接隔离的预览后端。
 * 未提供变量时继续使用项目默认端口，保证日常 `npm run dev` 的行为保持不变。
 */
const developmentProxyTarget = process.env.VITE_PROXY_TARGET || "http://127.0.0.1:19091";

/**
 * Vite 构建配置。
 *
 * 功能说明：
 * 将 React 控制台构建到 Go package 可 embed 的 `internal/api/dist`。
 *
 * 参数说明：
 * 无运行时参数；Vite 通过本文件读取配置。
 *
 * 返回值说明：
 * 返回 Vite 配置对象。
 *
 * 可能的异常/错误情况：
 * 构建时如果依赖缺失或输出目录不可写，Vite 会直接失败并返回非零退出码。
 */
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      "@": fileURLToPath(new URL("./src", import.meta.url)),
    },
  },
  build: {
    outDir: "../internal/api/dist",
    emptyOutDir: true,
  },
  server: {
    port: 5173,
    proxy: {
      "/api": developmentProxyTarget,
      "/ports": developmentProxyTarget,
      "/healthz": developmentProxyTarget,
    },
  },
});
