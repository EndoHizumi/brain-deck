import { defineConfig } from 'vitest/config'

// base を相対パスにして、GitHub Pages のようなサブディレクトリでも、
// ローカルの静的サーバーでも、そのまま動くようにする
export default defineConfig({
  base: './',
  server: {
    // 埋め込みフォントを、リポジトリの font/ から直接読む
    fs: { allow: ['..'] },
  },
  build: { outDir: 'dist', target: 'es2022' },
  test: { environment: 'jsdom', setupFiles: ['test/setup.ts'] },
})
