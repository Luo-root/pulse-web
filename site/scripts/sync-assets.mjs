/**
 * 把品牌资产从仓库的 `assets/` 同步到 `site/public/`。
 *
 * 事实源只有一个：`assets/logo.svg` / `assets/favicon.svg`（`assets_test.go` 守着它们
 * 与设计文档参数表的一致性）。站点这两份是**产物**，不进版本库（见 site/.gitignore），
 * 由 `predocs:dev` / `predocs:build` 自动生成——钩子名必须与 package.json 里的脚本名
 * 逐字对应，npm 只认 `pre<脚本名>`；这样就不存在「两处各维护一份」的漂移。
 */
import { copyFileSync, mkdirSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const assetsDir = resolve(here, '../../assets')
const publicDir = resolve(here, '../public')

const FILES = ['logo.svg', 'favicon.svg']

mkdirSync(publicDir, { recursive: true })
for (const name of FILES) {
  copyFileSync(join(assetsDir, name), join(publicDir, name))
  console.log(`sync-assets: assets/${name} → site/public/${name}`)
}
