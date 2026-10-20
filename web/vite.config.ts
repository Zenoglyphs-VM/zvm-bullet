import {existsSync, readFileSync} from 'node:fs'
import {defineConfig, loadEnv} from 'vite'

// Served by a ZVM gateway at /bullet/. BASE_PATH is honoured so
// zvm/gateway/build-sites.ps1 can set it the way it does for the other apps.
export default defineConfig(({mode}) => {
  // zvm-bullet/.env, the one settings file. Only VITE_ names are read, so the
  // bot's token beside them never reaches the page.
  const env = loadEnv(mode, '..', 'VITE_')
  // `||`, not `??`: the Dockerfile passes an unset build argument as '', and
  // that built a link to https://t.me/ with no bot in it.
  const bot = env.VITE_BULLET_BOT || 'zvm_bullet_bot'
  // The gateway the explorer and glyphswap are on: '' beside them, their
  // origin in a copy served elsewhere (GitHub Pages).
  const gw = env.VITE_GATEWAY || ''
  // The record the deploy wrote, so the page cannot name a contract the bot
  // does not use: deployments/<VITE_DEPLOYMENT, default zvm>.json, kept out of
  // git because every devnet deploys its own. Without one every address on the
  // page reads as not deployed.
  const file = new URL(`../deployments/${env.VITE_DEPLOYMENT || 'zvm'}.json`, import.meta.url)
  const none = 'not deployed on this chain'
  const record = existsSync(file)
    ? JSON.parse(readFileSync(file, 'utf8'))
    : {token: none, escrow: none, pair: none, bot: none, revenue: none, marketing: none, vrfPublicKey: none}

  const vars: Record<string, string> = {
    bot,
    token: record.token,
    escrow: record.escrow,
    pair: record.pair,
    botWallet: record.bot,
    revenue: record.revenue,
    marketing: record.marketing,
    vrfPublicKey: record.vrfPublicKey,
    tokenWrite: `${gw}/explorer/address/${record.token}?tab=contract`,
    buy: `${gw}/glyphswap/?in=znn&out=${record.token}`,
    gw,
  }

  return {
    base: process.env.BASE_PATH ?? '/bullet/',
    envDir: '..',
    server: {port: 47600, strictPort: true},
    plugins: [
      {
        name: 'bullet-links',
        transformIndexHtml: {
          order: 'pre',
          // A name with no value fails the build: a page shipped with a
          // literal {{name}} in an href is the dead link this replaced.
          handler: (html) =>
            html.replace(/\{\{(\w+)\}\}/g, (_, k: string) => {
              if (!(k in vars)) throw new Error(`index.html names {{${k}}}, which vite.config.ts does not define`)
              return vars[k]
            }),
        },
      },
    ],
  }
})
