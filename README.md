# zvm-bullet

[Bullet Game](https://bullet-game.gitbook.io/bullet-game), Russian Roulette in
Telegram for the `BULLET` token, on the ZVM. The token and the escrow are the
contracts the game ran on Ethereum, pulled from mainnet; the bot is new,
written to the original's documented rules.

The escrow is mainnet's code, unchanged; `npm run check:mainnet` proves every
executable byte matches. The token adds one constructor branch so it can deploy
on chain 7340469 at all.

- `contracts/`: the mainnet sources, the ZVM port, and their checks
- `cmd/zvm-bullet`: `keys`, `deploy`, `check`, `bot` and `verify`
- `web/`: the rules, the fairness proof and the addresses, as a page
- `deployments/zvm.json`: what a deploy wrote; kept out of git, as every devnet deploys its own

## Run

```sh
cd contracts && npm install && npm run check:mainnet && npm run build
go test ./...
cp .env.example .env              # the Telegram token, chats, RPC
docker compose up -d --build      # the bot, and the page on 127.0.0.1:47601
```

`.env` and `secrets/` hold the escrow owner's key and the bot's token, and stay
out of git and out of both images.

## Deploy the page

Every push to `main` publishes `web/` to GitHub Pages (Settings, Pages, Source:
GitHub Actions), with its links pointed at the organisation's `ZVM_GATEWAY` Actions variable.

No node is built in: the bot and the deploy commands read `ZVM_RPC` (and
`ZVM_NODE` to verify), the page `VITE_*`. `.env.example` has
a local [zvm-devnet](https://github.com/Zenoglyphs-VM/zvm-devnet)'s.

MIT licence; the original contracts are noted in [NOTICE.md](NOTICE.md). The full guide, with how it works and why: [docs/GUIDE.md](docs/GUIDE.md).
