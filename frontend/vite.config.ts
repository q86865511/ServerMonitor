// 用 vitest/config 的 defineConfig(vite 原生 defineConfig 的超集,補上 `test` 欄位型別)
// 取代 vite 版,單一設定檔同時餵給 `vite build`/`wails dev` 與 `vitest run`——vite CLI 不認得
// `test` 欄位,原樣忽略,不影響既有建置行為。
import {defineConfig} from 'vitest/config'
import {svelte} from '@sveltejs/vite-plugin-svelte'

// https://vitejs.dev/config/
export default defineConfig({
  plugins: [svelte()],
  build: {
    // 避免清空被版控追蹤的 dist/.gitkeep——該檔是 fresh clone 建置的前提(main.go 的
    // //go:embed 需要 dist 目錄存在),vite 預設會在建置前清空 outDir 把它一併刪掉。
    emptyOutDir: false
  },
  test: {
    // store 純邏輯測試,不涉及實際 DOM——用 node 環境即可,不需引入 jsdom。
    environment: 'node',
    include: ['src/**/*.test.ts']
  }
})
