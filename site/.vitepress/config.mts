import { defineConfig } from 'vitepress'

const REPO = 'https://github.com/Luo-root/pulse-web'
const DESIGN_DOC = `${REPO}/blob/main/docs/design/web-framework-design.md`

// 站点是**公开展示面**：怎么用、多快、怎么落地在这里；
// 「为什么这么设计」与全部实测数据在设计文档（事实源），不在这里重述。
// 语言结构对齐上游 pulse：根 = 中文，`/en/` = English。
export default defineConfig({
  base: '/pulse-web/',
  title: 'Pulse-Web',
  description: '通用 Go web 框架：装配内核 + 一等观测',

  cleanUrls: true,
  appearance: true, // 跟随系统 + 手动切换 + localStorage 持久化

  head: [
    ['link', { rel: 'icon', type: 'image/svg+xml', href: '/pulse-web/favicon.svg' }],
    ['meta', { name: 'theme-color', content: '#3d7ab8' }],
  ],

  locales: {
    root: {
      label: '中文',
      lang: 'zh-CN',
      description: '基于 pulse 的 kernel 与 observability 构建的通用 Go web 框架',
      themeConfig: {
        nav: [
          { text: '指南', link: '/guide/getting-started' },
          { text: '观测', link: '/guide/observability' },
          { text: '性能', link: '/performance' },
          { text: '设计文档', link: DESIGN_DOC },
        ],
        sidebar: {
          '/': [
            {
              text: '指南',
              items: [
                { text: '快速开始', link: '/guide/getting-started' },
                { text: '路由与中间件', link: '/guide/routing' },
                { text: '请求', link: '/guide/requests' },
                { text: '响应', link: '/guide/responses' },
                { text: '错误模型', link: '/guide/errors' },
                { text: '观测', link: '/guide/observability' },
                { text: '装配与运行', link: '/guide/assembly' },
                { text: '测试', link: '/guide/testing' },
                { text: '落地与契约', link: '/guide/ops-contracts' },
              ],
            },
            {
              text: '数据',
              items: [{ text: '性能', link: '/performance' }],
            },
          ],
        },
        outline: { label: '本页目录', level: [2, 3] },
        docFooter: { prev: '上一篇', next: '下一篇' },
        returnToTopLabel: '回到顶部',
        sidebarMenuLabel: '目录',
        darkModeSwitchLabel: '外观',
        lightModeSwitchTitle: '切换到浅色',
        darkModeSwitchTitle: '切换到深色',
        langMenuLabel: '切换语言',
        lastUpdated: {
          text: '最后更新于',
          formatOptions: { dateStyle: 'short', timeStyle: 'short' },
        },
      },
    },
    en: {
      label: 'English',
      lang: 'en',
      link: '/en/',
      description: 'A general-purpose Go web framework built on pulse kernel + observability',
      themeConfig: {
        nav: [
          { text: 'Guide', link: '/en/guide/getting-started' },
          { text: 'Observability', link: '/en/guide/observability' },
          { text: 'Performance', link: '/en/performance' },
          { text: 'Design doc', link: DESIGN_DOC },
        ],
        sidebar: {
          '/en/': [
            {
              text: 'Guide',
              items: [
                { text: 'Getting started', link: '/en/guide/getting-started' },
                { text: 'Routing and middleware', link: '/en/guide/routing' },
                { text: 'Requests', link: '/en/guide/requests' },
                { text: 'Responses', link: '/en/guide/responses' },
                { text: 'The error model', link: '/en/guide/errors' },
                { text: 'Observability', link: '/en/guide/observability' },
                { text: 'Assembly and running', link: '/en/guide/assembly' },
                { text: 'Testing', link: '/en/guide/testing' },
                { text: 'Deployment and contracts', link: '/en/guide/ops-contracts' },
              ],
            },
            {
              text: 'Data',
              items: [{ text: 'Performance', link: '/en/performance' }],
            },
          ],
        },
        outline: { label: 'On this page', level: [2, 3] },
        docFooter: { prev: 'Previous', next: 'Next' },
        returnToTopLabel: 'Return to top',
        sidebarMenuLabel: 'Menu',
        darkModeSwitchLabel: 'Appearance',
        lightModeSwitchTitle: 'Switch to light theme',
        darkModeSwitchTitle: 'Switch to dark theme',
        langMenuLabel: 'Change language',
        lastUpdated: {
          text: 'Last updated',
          formatOptions: { dateStyle: 'short', timeStyle: 'short' },
        },
      },
    },
  },

  themeConfig: {
    logo: '/logo.svg',
    siteTitle: 'Pulse-Web',
    socialLinks: [{ icon: 'github', link: REPO }],
    search: { provider: 'local' },
    editLink: {
      pattern: `${REPO}/edit/main/site/:path`,
      text: '在 GitHub 上编辑此页',
    },
    footer: {
      message: 'MIT 许可 · 核心模块零第三方依赖（仅标准库 + pulse kernel / observability）',
    },
  },
})
