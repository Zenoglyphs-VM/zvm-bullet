import assert from 'node:assert/strict'
import {readFileSync} from 'node:fs'
import test from 'node:test'
import {compile} from '../scripts/solc.mjs'
import {artifacts, BURN, COMMITMENT, DEADLINE, fixture, fixtureArtifacts, INITIAL, LIQUIDITY, MAX, MIN,
  originalMainnet, pairHash, REVENUE, stripMetadata, SUPPLY, upstream, ZERO} from './evm.mjs'

const own = 'Ownable: caller is not the owner'
const args = (f, result, name, at = f.escrow) => f.events(result, at).filter((e) => e.eventName === name).map((e) => e.args)
async function ready() {
  const f = await fixture({launch: true, renounce: true}); await f.fundPlayers(); return f
}
function capped(bets) {
  const total = bets.reduce((a, b) => a + b, 0n)
  return bets.map((bet) => bet < total - bet ? bet : total - bet)
}
function settlement(bets, loser) {
  const stake = bets[loser], burn = stake * BURN / 10_000n, budget = stake - burn - stake * REVENUE / 10_000n
  const weight = bets.reduce((a, b, i) => a + (i === loser ? 0n : b), 0n)
  const profits = bets.map((bet, i) => i === loser ? 0n : budget * bet / weight)
  return {burn, profits, revenue: stake - burn - profits.reduce((a, b) => a + b, 0n)}
}

test('fresh production compilation matches both complete tracked artifacts and literal LF sources', () => {
  for (const [name, artifact] of Object.entries(artifacts)) {
    assert.deepEqual(artifact, JSON.parse(readFileSync(new URL('../../internal/contracts/artifacts/' + name + '.json', import.meta.url))))
    assert.equal(artifact.standardJsonInput.settings.optimizer.runs, 1_000_000)
    assert.equal(artifact.standardJsonInput.settings.evmVersion, 'paris')
    assert.equal(artifact.standardJsonInput.sources[name + '.sol'].content.includes('\r\n'), false)
  }
  assert.equal(upstream.commit, 'ae90b945b0ea95887c31cfb2f19e0b88504186d8')
  assert.equal(upstream.counterparts_recompiled_by_this_suite, false)
  assert.equal(pairHash, '0x96e8ac4277198ff8b6f785478aa9a39f403cb768dd02cbee326c3e7da348845f')
})

test('fresh original mainnet sources match the recorded creation inputs excluding only terminal metadata', () => {
  for (const name of ['BulletGame', 'TelegramRussianRoulette']) {
    const {output} = compile(name + '.sol', new URL('../mainnet/' + name + '.sol', import.meta.url), name)
    const saved = originalMainnet[name], bytes = saved.creationBytecode.replace(/^0x/, '').toLowerCase(), tail = (saved.constructorArgs ?? '').toLowerCase()
    assert.ok(!tail || bytes.endsWith(tail))
    assert.equal(stripMetadata(output.evm.bytecode.object), stripMetadata(bytes.slice(0, bytes.length - tail.length)))
  }
  // This compares stored inputs; no Ethereum lookup is performed.
})

test('actual constructors initialize token/escrow roles and use the explicit constructed-router code alias', async () => {
  const f = await fixture()
  assert.notEqual(f.router, f.constructedRouter)
  assert.equal(await f.read(f.token, 'router'), f.router)
  assert.equal(await f.read(f.token, 'factory'), f.factory)
  assert.equal(await f.read(f.token, 'owner'), f.owner)
  assert.equal(await f.read(f.token, 'decimals'), 8)
  assert.equal(await f.read(f.token, 'name'), 'Bullet Game Betting Token')
  assert.equal(await f.read(f.token, 'symbol'), 'BULLET')
  assert.equal(await f.read(f.token, 'totalSupply'), 0n)
  assert.equal(await f.read(f.token, 'allowance', [f.token, f.router]), MAX)
  assert.equal(await f.read(f.token, 'isLaunched'), false)
  assert.equal(await f.read(f.escrow, 'owner'), f.owner)
  assert.equal(await f.read(f.escrow, 'bettingToken'), f.token)
  assert.equal(await f.read(f.escrow, 'minimumBet'), MIN)
  assert.equal(await f.read(f.escrow, 'revenueBps'), REVENUE)
  assert.equal(await f.read(f.escrow, 'burnBps'), BURN)
  await f.conserved()
})

test('chain selection accepts the declared branches and preserves the existing testnet mint policy', async (t) => {
  for (const chainId of [1, 5, 11155111, 7340469]) await t.test('chain ' + chainId, async () => {
    const f = await fixture({chainId}), allowed = chainId === 5 || chainId === 11155111
    assert.equal(await f.read(f.token, 'isTestnet'), allowed)
    if (allowed) {
      await f.write(f.alice, f.token, 'mint', [1000n]); assert.equal(await f.read(f.token, 'balanceOf', [f.alice]), 1000n)
    } else await f.write(f.alice, f.token, 'mint', [1000n], 0n, {error: 'not testnet'})
    await f.conserved()
  })
  await fixture({chainId: 1337, tokenError: 'expected mainnet'})
})

test('wallet setters require the owner and a nonzero address', async () => {
  const f = await fixture()
  for (const name of ['setMyWallet', 'setMarketingWallet', 'setRevenueWallet', 'setRouletteContract']) {
    await f.write(f.outsider, f.token, name, [f.alice], 0n, {error: own})
    await f.write(f.owner, f.token, name, [ZERO], 0n, {error: 'null address'})
  }
  await f.configure()
  for (const [name, value] of [['myWallet', f.wallet], ['marketingWallet', f.marketing], ['revenueWallet', f.revenue], ['rouletteContract', f.escrow]])
    assert.equal(await f.read(f.token, name), value)
})

test('the one-shot anti-bot switches record the actual tax rates and events', async () => {
  const f = await fixture()
  await f.write(f.outsider, f.token, 'enableAntiBotMode', [], 0n, {error: own})
  assert.equal(args(f, await f.write(f.owner, f.token, 'enableAntiBotMode'), 'AntiBotEngaged', f.token).length, 1)
  assert.equal(await f.read(f.token, 'buyTaxBps'), 1000n)
  assert.equal(await f.read(f.token, 'sellTaxBps'), 1000n)
  await f.write(f.owner, f.token, 'enableAntiBotMode', [], 0n, {error: 'this is a one shot function'})
  assert.equal(args(f, await f.write(f.owner, f.token, 'disableAntiBotMode'), 'AntiBotDisengaged', f.token).length, 1)
  assert.equal(await f.read(f.token, 'buyTaxBps'), 500n)
  assert.equal(await f.read(f.token, 'sellTaxBps'), 500n)
  await f.write(f.owner, f.token, 'disableAntiBotMode', [], 0n, {error: 'this is a one shot function'})
})

test('incomplete and zero-liquidity launches revert all mints and value before a valid actual V2 launch', async () => {
  const f = await fixture()
  await f.write(f.outsider, f.token, 'stealthLaunch', [], LIQUIDITY, {error: own})
  await f.write(f.owner, f.token, 'stealthLaunch', [], LIQUIDITY, {error: 'null address'})
  await f.configure()
  await f.write(f.owner, f.token, 'stealthLaunch', [], 0n, {error: true})
  assert.equal(await f.read(f.token, 'isLaunched'), false)
  assert.equal(await f.read(f.token, 'totalSupply'), 0n)
  const result = await f.launchToken()
  assert.equal(args(f, result, 'StealthLaunchEngaged', f.token).length, 1)
  assert.equal(await f.read(f.token, 'totalSupply'), SUPPLY)
  assert.equal(await f.read(f.token, 'balanceOf', [f.marketing]), SUPPLY / 10n)
  assert.deepEqual(await f.reserves(), {tokens: SUPPLY * 9n / 10n, native: LIQUIDITY})
  assert.equal(await f.read(f.factory, 'getPair', [f.token, f.wznn]), f.pair)
  assert.equal(await f.read(f.pair, 'balanceOf', [ZERO]), 1000n)
  assert.ok(await f.read(f.pair, 'balanceOf', [f.owner]) > 0n)
  await f.write(f.owner, f.token, 'stealthLaunch', [], LIQUIDITY, {error: 'already launched'})
  await f.conserved()
})

test('ownership transfer and renunciation revoke token administration while escrow ownership stays separate', async () => {
  const f = await fixture()
  await f.write(f.outsider, f.token, 'transferOwnership', [f.alice], 0n, {error: own})
  await f.write(f.owner, f.token, 'transferOwnership', [f.alice])
  await f.write(f.owner, f.token, 'setMyWallet', [f.wallet], 0n, {error: own})
  await f.write(f.alice, f.token, 'renounceOwnership')
  assert.equal(await f.read(f.token, 'owner'), ZERO)
  assert.equal(await f.read(f.escrow, 'owner'), f.owner)
  await f.write(f.alice, f.token, 'setMyWallet', [f.wallet], 0n, {error: own})
})

test('connectAndApprove grants the actual escrow an infinite allowance and finite transfers decrement correctly', async () => {
  const f = await ready(), before = await f.read(f.token, 'balanceOf', [f.alice])
  assert.equal(await f.read(f.token, 'allowance', [f.alice, f.escrow]), MAX)
  await f.write(f.alice, f.token, 'approve', [f.bob, 9n])
  await f.write(f.bob, f.token, 'transferFrom', [f.alice, f.carol, 10n], 0n, {error: true})
  await f.write(f.bob, f.token, 'transferFrom', [f.alice, f.carol, 9n])
  assert.equal(await f.read(f.token, 'allowance', [f.alice, f.bob]), 0n)
  assert.equal(await f.read(f.token, 'balanceOf', [f.alice]), before - 9n)
  await f.write(f.alice, f.token, 'transfer', [f.bob, 1n])
  await f.conserved()
})

test('expired and invalid permits fail without consuming a nonce or changing approval', async () => {
  const f = await fixture(), zero = '0x' + '00'.repeat(32)
  for (const [deadline, error] of [[0n, 'PERMIT_DEADLINE_EXPIRED'], [DEADLINE, 'INVALID_SIGNER']]) {
    await f.write(f.outsider, f.token, 'permit', [f.alice, f.bob, 99n, deadline, 27, zero, zero], 0n, {error})
    assert.equal(await f.read(f.token, 'nonces', [f.alice]), 0n)
    assert.equal(await f.read(f.token, 'allowance', [f.alice, f.bob]), 0n)
  }
})

test('actual V2 buys match independent reserve math and the 5% tax split', async () => {
  const f = await ready(), before = await f.read(f.token, 'balanceOf', [f.alice]), r = await f.reserves(), paid = 10n ** 16n
  const gross = paid * 997n * r.tokens / (r.native * 1000n + paid * 997n), tax = gross * 500n / 10_000n, revenue = tax / 5n
  await f.buy(f.alice, paid)
  assert.equal(await f.read(f.token, 'balanceOf', [f.alice]), before + gross - tax)
  assert.equal(await f.read(f.token, 'balanceOf', [f.revenue]), revenue)
  assert.equal(await f.read(f.token, 'balanceOf', [f.token]), tax - revenue)
  assert.deepEqual(await f.reserves(), {tokens: r.tokens - gross, native: r.native + paid})
  await f.conserved()
})

test('actual V2 sells use the after-tax input, enforce approval and pay the reserve-model output', async () => {
  const f = await ready(), amount = 1_000_000n, r = await f.reserves(), before = await f.balance(f.alice)
  const call = () => f.write(f.alice, f.router, 'swapExactTokensForETHSupportingFeeOnTransferTokens', [amount, 0n, [f.token, f.wznn], f.alice, DEADLINE])
  await f.write(f.alice, f.router, 'swapExactTokensForETHSupportingFeeOnTransferTokens', [amount, 0n, [f.token, f.wznn], f.alice, DEADLINE], 0n, {error: true})
  await f.write(f.alice, f.token, 'approve', [f.router, amount])
  const net = amount - amount * 500n / 10_000n, output = net * 997n * r.native / (r.tokens * 1000n + net * 997n)
  await call()
  assert.equal(await f.balance(f.alice), before + output)
  assert.equal(await f.read(f.token, 'allowance', [f.alice, f.router]), 0n)
  assert.deepEqual(await f.reserves(), {tokens: r.tokens + net, native: r.native - output})
  await f.conserved()
})

test('tax floors at small amounts preserve the complete supply inventory', async () => {
  const f = await ready()
  await f.write(f.alice, f.token, 'transfer', [f.pair, 19n])
  assert.equal(await f.read(f.token, 'balanceOf', [f.token]), 0n)
  await f.write(f.alice, f.token, 'transfer', [f.pair, 20n])
  assert.equal(await f.read(f.token, 'balanceOf', [f.token]), 1n)
  assert.equal(await f.read(f.token, 'balanceOf', [f.revenue]), 0n)
  await f.write(f.outsider, f.pair, 'sync')
  await f.conserved()
})

test('collected tax crosses the threshold and a later actual call swaps and adds real V2 liquidity', async () => {
  const f = await ready(), threshold = SUPPLY * 2n / 10_000n
  for (let i = 0; i < 8; i++) await f.buy(f.alice, 10n ** 16n)
  assert.ok(await f.read(f.token, 'balanceOf', [f.token]) > threshold)
  assert.equal(await f.read(f.token, 'owner'), ZERO)
  const lp = await f.read(f.pair, 'balanceOf', [f.owner]), burnedLp = await f.read(f.pair, 'balanceOf', [ZERO])
  const supply = await f.read(f.pair, 'totalSupply'), wallet = await f.balance(f.wallet)
  await f.write(f.alice, f.token, 'transfer', [f.bob, 0n])
  assert.equal(await f.read(f.pair, 'balanceOf', [f.owner]), lp)
  const minted = await f.read(f.pair, 'totalSupply') - supply
  assert.ok(minted > 0n)
  assert.equal(await f.read(f.pair, 'balanceOf', [ZERO]), burnedLp + minted)
  assert.ok(await f.balance(f.wallet) > wallet)
  assert.equal(await f.balance(f.token), 0n)
  assert.ok(await f.read(f.token, 'balanceOf', [f.token]) < threshold)
  await f.conserved()
})

for (const [name, options, error] of [
  ['small revolver', () => ({size: 1n}), 'Revolver size too small'],
  ['too many players', () => ({size: 2n}), 'Too many players for this size revolver'],
  ['low game minimum', () => ({min: MIN - 1n}), 'Minimum bet too small'],
  ['mismatched arrays', () => ({bets: [1000n]}), 'Players/bets length mismatch'],
  ['one player', (f) => ({players: [f.alice], bets: [1000n]}), 'Not enough players'],
  ['low player bet', () => ({bets: [999n, 2000n, 3000n]}), 'Bet is smaller than the minimum'],
]) test('newGame rejects ' + name + ' with complete state rollback', async () => {
  const f = await ready(); await f.start({...options(f), error}); await f.start(); await f.abort(); await f.conserved()
})

test('only the escrow owner starts, settles or aborts a game', async () => {
  const f = await ready()
  await f.start({from: f.outsider, error: own}); await f.start()
  for (const [name, values] of [['endGame', [-1n, 0, []]], ['abortGame', [-1n]], ['abortAllGames', []]])
    await f.write(f.outsider, f.escrow, name, values, 0n, {error: own})
  await f.abort(); await f.conserved()
})

test('game creation caps an oversized bet, returns exact bets and records actual escrow balances/events', async () => {
  const f = await ready(), players = [f.alice, f.bob, f.carol], bets = [10_000n, 1000n, 2000n], result = await f.start({bets})
  assert.deepEqual(result.bets, capped(bets))
  assert.deepEqual(args(f, result, 'Bet'), players.map((player, i) => ({tgChatId: -1n, player, playerIndex: i, amount: result.bets[i]})))
  assert.equal(await f.read(f.token, 'balanceOf', [f.escrow]), 6000n)
  assert.equal(await f.read(f.escrow, 'isGameInProgress', [-1n]), true)
  assert.equal(await f.read(f.escrow, 'activeTgGroups', [0n]), -1n)
  const game = await f.read(f.escrow, 'games', [-1n])
  assert.deepEqual(game, [6n, MIN, COMMITMENT, true, 0])
  await f.start({error: 'There is already a game in progress'})
  await f.abort(); await f.conserved()
})

test('insufficient allowance or a later insufficient balance reverses all earlier player collections', async () => {
  const f = await ready()
  await f.write(f.bob, f.token, 'approve', [f.escrow, 999n])
  await f.start({error: 'Not enough allowance'})
  await f.write(f.bob, f.token, 'connectAndApprove', [7])
  const have = await f.read(f.token, 'balanceOf', [f.bob])
  await f.write(f.bob, f.token, 'burn', [have])
  await f.start({error: true})
  assert.equal(await f.read(f.token, 'balanceOf', [f.escrow]), 0n)
  assert.equal(await f.read(f.escrow, 'isGameInProgress', [-1n]), false)
  await f.conserved()
})

test('settlement pays independent proportional profits, burns supply and assigns all rounding residue to revenue', async (t) => {
  for (const loser of [0, 1, 2]) await t.test('loser ' + loser, async () => {
    const f = await ready(), players = [f.alice, f.bob, f.carol], bets = [1001n, 2003n, 3004n], paid = capped(bets), model = settlement(paid, loser)
    const before = await Promise.all(players.map((at) => f.read(f.token, 'balanceOf', [at])))
    await f.start({bets}); const result = await f.finish(-1n, loser)
    for (let i = 0; i < 3; i++) assert.equal(await f.read(f.token, 'balanceOf', [players[i]]), before[i] + (i === loser ? -paid[i] : model.profits[i]))
    assert.deepEqual(args(f, result, 'Win'), players.flatMap((player, i) => i === loser ? [] : [{tgChatId: -1n, player, playerIndex: i, amount: model.profits[i]}]))
    assert.deepEqual(args(f, result, 'Burn'), [{tgChatId: -1n, amount: model.burn}])
    assert.deepEqual(args(f, result, 'Revenue'), [{tgChatId: -1n, amount: model.revenue}])
    assert.equal(await f.read(f.token, 'balanceOf', [f.revenue]), model.revenue)
    assert.equal(await f.read(f.token, 'totalSupply'), SUPPLY - model.burn)
    assert.equal(await f.read(f.token, 'balanceOf', [f.escrow]), 0n)
    await f.finish(-1n, loser, {error: 'No game in progress for this Telegram chat ID'})
    await f.conserved()
  })
})

test('sentinel and out-of-range losers fail without closing or changing the funded game', async () => {
  const f = await ready(); await f.start()
  await f.finish(-1n, 65535, {error: "Loser index shouldn't be the sentinel value"})
  await f.finish(-1n, 3, {error: 'Loser index out of range'})
  assert.equal(await f.read(f.escrow, 'isGameInProgress', [-1n]), true)
  await f.abort(); await f.conserved()
})

test('abort, active-list swap removal and abortAll refund concurrent games and permit a later game', async () => {
  const f = await ready(), players = [f.alice, f.bob, f.carol], before = await Promise.all(players.map((at) => f.read(f.token, 'balanceOf', [at])))
  for (const id of [-1n, -2n, -3n]) await f.start({id})
  await f.abort(-2n)
  assert.deepEqual(await Promise.all([0n, 1n].map((i) => f.read(f.escrow, 'activeTgGroups', [i]))), [-1n, -3n])
  await f.write(f.owner, f.escrow, 'abortAllGames')
  for (let i = 0; i < 3; i++) assert.equal(await f.read(f.token, 'balanceOf', [players[i]]), before[i])
  await f.abort(-1n, {error: 'No game in progress for this Telegram chat ID'})
  await f.start(); await f.abort(); await f.conserved()
})

test('false replies during collection, later payout, revenue or abort restore all game and token state', async (t) => {
  for (const operation of ['collection', 'winner', 'revenue', 'abort']) await t.test(operation, async () => {
    const f = await fixture(), token = await f.deploy(fixtureArtifacts.FailingToken), at = await f.newEscrow({bettingToken: token})
    for (const player of [f.alice, f.bob, f.carol]) {
      await f.write(f.owner, token, 'mint', [player, 10_000n]); await f.write(player, token, 'approve', [at, MAX])
    }
    if (operation === 'collection') {
      await f.write(f.owner, token, 'configure', [2n])
      await f.start({at, error: 'Funds transfer failed'})
    } else {
      await f.start({at})
      await f.write(f.owner, token, 'configure', [operation === 'revenue' ? 3n : 2n])
      if (operation === 'abort') await f.write(f.owner, at, 'abortGame', [-1n], 0n, {error: 'Funds transfer failed'})
      else await f.write(f.owner, at, 'endGame', [-1n, 0, []], 0n, {error: operation === 'revenue' ? 'Revenue transfer failed' : 'Funds transfer failed'})
      assert.equal(await f.read(at, 'isGameInProgress', [-1n]), true)
    }
    assert.equal(await f.read(token, 'calls'), 0n)
    await f.write(f.owner, token, 'configure', [0n])
    if (operation === 'collection') await f.start({at})
    await f.write(f.owner, at, 'abortGame', [-1n])
    for (const player of [f.alice, f.bob, f.carol]) assert.equal(await f.read(token, 'balanceOf', [player]), 10_000n)
    await f.conserved()
  })
})

function random(seed) {
  let state = seed >>> 0
  return () => {state ^= state << 13; state ^= state >>> 17; state ^= state << 5; return state >>> 0}
}
test('seeded concurrent game histories match independent balances, supply, liabilities and decoded settlement receipts', async (t) => {
  for (const seed of [3, 17, 0x12345678, 0x9e3779b9]) await t.test('seed ' + seed, async () => {
    const f = await ready(), next = random(seed), players = [f.alice, f.bob, f.carol, f.outsider]
    const balances = new Map(players.map((at) => [at, 100_000_000_000n])), active = new Map(), receipts = new Map()
    let supply = SUPPLY, revenue = 0n, settled = 0, aborted = 0
    for (let step = 0; step < 32; step++) {
      const id = -100n - BigInt(next() % 4), game = active.get(id)
      if (!game) {
        const count = 2 + next() % 3, members = players.slice(0, count), bets = members.map(() => MIN + BigInt(next() % 50_000)), paid = capped(bets)
        const result = await f.start({id, players: members, bets})
        assert.deepEqual(result.bets, paid)
        for (let i = 0; i < count; i++) balances.set(members[i], balances.get(members[i]) - paid[i])
        active.set(id, {members, paid})
        receipts.set(id, args(f, result, 'Bet').map((e) => ({member: e.player, amount: e.amount})))
        assert.deepEqual(receipts.get(id), members.map((member, i) => ({member, amount: paid[i]})))
        await f.start({id, error: 'There is already a game in progress'})
      } else {
        if (next() % 3 === 0) {
          await f.abort(id); aborted++
          for (let i = 0; i < game.members.length; i++) balances.set(game.members[i], balances.get(game.members[i]) + game.paid[i])
        } else {
          const loser = next() % game.members.length, model = settlement(game.paid, loser), result = await f.finish(id, loser)
          for (let i = 0; i < game.members.length; i++) if (i !== loser) balances.set(game.members[i], balances.get(game.members[i]) + game.paid[i] + model.profits[i])
          supply -= model.burn; revenue += model.revenue; settled++
          assert.deepEqual(args(f, result, 'Win'), game.members.flatMap((player, i) => i === loser ? [] : [{tgChatId: id, player, playerIndex: i, amount: model.profits[i]}]))
          assert.equal(args(f, result, 'Revenue')[0].amount, model.revenue)
          assert.equal(args(f, result, 'Burn')[0].amount, model.burn)
        }
        active.delete(id); receipts.delete(id)
      }
      for (const at of players) assert.equal(await f.read(f.token, 'balanceOf', [at]), balances.get(at))
      assert.equal(await f.read(f.token, 'balanceOf', [f.revenue]), revenue)
      assert.equal(await f.read(f.token, 'totalSupply'), supply)
      const liabilities = [...active.values()].flatMap((g) => g.paid).reduce((a, b) => a + b, 0n)
      assert.equal(await f.read(f.token, 'balanceOf', [f.escrow]), liabilities)
      const fromLogs = [...receipts.values()].flat().reduce((sum, e) => sum + e.amount, 0n)
      assert.equal(fromLogs, liabilities)
      for (const id of [-100n, -101n, -102n, -103n]) assert.equal(await f.read(f.escrow, 'isGameInProgress', [id]), active.has(id))
      const onchain = await Promise.all(Array.from({length: active.size}, (_, i) => f.read(f.escrow, 'activeTgGroups', [BigInt(i)])))
      assert.deepEqual(new Set(onchain), new Set(active.keys()))
      await f.conserved()
    }
    assert.ok(settled > 0 && aborted > 0)
    await f.write(f.owner, f.escrow, 'abortAllGames')
    assert.equal(await f.read(f.token, 'balanceOf', [f.escrow]), 0n)
    await f.conserved()
  })
})
