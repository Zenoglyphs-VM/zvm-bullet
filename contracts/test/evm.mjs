import assert from 'node:assert/strict'
import {createHash} from 'node:crypto'
import {readFileSync} from 'node:fs'
import {createBlock} from '@ethereumjs/block'
import {createCustomCommon, Hardfork, Mainnet} from '@ethereumjs/common'
import {createAccount, createAddressFromString, bytesToHex, hexToBytes} from '@ethereumjs/util'
import {createVM} from '@ethereumjs/vm'
import {decodeErrorResult, decodeEventLog, decodeFunctionResult, encodeDeployData, encodeFunctionData, getAddress, keccak256} from 'viem'
import solc from 'solc'
import {compile, SETTINGS, stripMetadata, version} from '../scripts/solc.mjs'

export const ZERO = '0x0000000000000000000000000000000000000000', MAX = (1n << 256n) - 1n
export const INITIAL = 10n ** 30n, SUPPLY = 10_000_000n * 10n ** 8n, LIQUIDITY = 10n ** 19n
export const MIN = 1000n, REVENUE = 900n, BURN = 100n, DEADLINE = 1_900_001_000n
export const COMMITMENT = '0x' + '11'.repeat(32)
const file = (name) => readFileSync(new URL(name, import.meta.url))
const prefix = (s) => s.startsWith('0x') ? s : '0x' + s
assert.match(version(), /^0\.8\.19\+commit\.7dd6d404\./)
assert.deepEqual(SETTINGS.optimizer, {enabled: true, runs: 1_000_000})
export const artifacts = {}
for (const [name, source] of [['BulletGame', '../zvm/BulletGame.sol'], ['TelegramRussianRoulette', '../mainnet/TelegramRussianRoulette.sol']]) {
  const {input, output: c} = compile(name + '.sol', new URL(source, import.meta.url), name)
  artifacts[name] = {contractName: name, sourceName: name + '.sol', compiler: version(), compilerVersion: 'v0.8.19+commit.7dd6d404',
    abi: c.abi, bytecode: prefix(c.evm.bytecode.object), deployedBytecode: prefix(c.evm.deployedBytecode.object), standardJsonInput: input}
  assert.ok(c.evm.deployedBytecode.object.length / 2 <= 24_576)
}
export const originalMainnet = JSON.parse(file('../mainnet/mainnet.json'))
export const upstream = JSON.parse(file('./upstream-contracts/provenance.json'))
for (const [name, pin] of Object.entries(upstream.files)) {
  const bytes = file('./upstream-contracts/' + name)
  assert.equal(bytes.length, pin.bytes); assert.equal(createHash('sha256').update(bytes).digest('hex'), pin.sha256)
}
export const counterparts = JSON.parse(file('./upstream-contracts/contracts.json'))
export const pairHash = keccak256(prefix(counterparts.UniswapV2Pair.bytecode))
const fixtureOutput = JSON.parse(solc.compile(JSON.stringify({language: 'Solidity',
  sources: {'Fixtures.sol': {content: file('./Fixtures.sol').toString()}}, settings: {evmVersion: 'paris', optimizer: {enabled: true, runs: 200},
    outputSelection: {'*': {'*': ['abi', 'evm.bytecode.object']}}}})))
assert.deepEqual((fixtureOutput.errors ?? []).filter((e) => e.severity === 'error'), [])
export const fixtureArtifacts = Object.fromEntries(Object.entries(fixtureOutput.contracts['Fixtures.sol']).map(([name, c]) =>
  [name, {abi: c.abi, bytecode: prefix(c.evm.bytecode.object)}]))
export {stripMetadata}

export async function fixture({chainId = 7340469, tokenError, launch = false, renounce = false} = {}) {
  // Osaka executes the retained WZNN. Bullet remains freshly compiled for Paris.
  const common = createCustomCommon({chainId}, Mainnet, {hardfork: Hardfork.Osaka}), vm = await createVM({common})
  const actors = Array.from({length: 8}, (_, n) => getAddress('0x' + (0x10000n + BigInt(n)).toString(16).padStart(40, '0')))
  const [owner, alice, bob, carol, marketing, revenue, wallet, outsider] = actors, address = createAddressFromString
  const inventory = new Set([...actors, ZERO].map((at) => at.toLowerCase())), abis = new Map(), deployments = []
  for (const at of actors) await vm.stateManager.putAccount(address(at), createAccount({balance: INITIAL}))
  const root = async () => bytesToHex(await vm.stateManager.getStateRoot())
  const balance = async (at) => (await vm.stateManager.getAccount(address(at)))?.balance ?? 0n
  async function frame({from = owner, to, data = '0x', value = 0n, gas = 50_000_000n, isStatic = false, error}) {
    vm.stateManager.originalStorageCache.clear(); vm.evm.journal.cleanJournal()
    const before = to ? await root() : undefined
    const block = createBlock({header: {number: 100n, timestamp: 1_900_000_100n, gasLimit: 60_000_000n}}, {common})
    // Unsigned message frames: no intrinsic gas, fees, inclusion or ZVM.
    const result = await vm.evm.runCall({caller: address(from), ...(to ? {to: address(to)} : {}), data: hexToBytes(data),
      value, gasLimit: gas, isStatic, block, skipNonceIncrement: Boolean(to)})
    const returned = bytesToHex(result.execResult.returnValue), failed = Boolean(result.execResult.exceptionError)
    if (error !== undefined) {
      assert.ok(failed, 'expected actual EVM failure')
      if (typeof error === 'string') assert.equal(decodeErrorResult({abi: artifacts.BulletGame.abi, data: returned}).args[0], error)
      if (to) assert.equal(await root(), before, 'a failed frame restores the complete state root and balances')
      assert.equal(result.execResult.logs?.length ?? 0, 0)
    } else assert.equal(result.execResult.exceptionError, undefined, `EVM frame failed: ${returned}`)
    return {...result, returned, failed}
  }
  async function deploy(code, args = [], options = {}) {
    const result = await frame({data: encodeDeployData({abi: code.abi, bytecode: prefix(code.bytecode), args}), ...options})
    if (result.failed) return result
    const at = getAddress(result.createdAddress.toString())
    assert.ok((await vm.stateManager.getCode(address(at))).length > 0)
    abis.set(at.toLowerCase(), code.abi); inventory.add(at.toLowerCase()); deployments.push({address: at, result}); return at
  }
  async function write(from, to, name, args = [], value = 0n, options = {}) {
    return frame({from, to, data: encodeFunctionData({abi: abis.get(to.toLowerCase()), functionName: name, args}), value, ...options})
  }
  async function read(to, name, args = []) {
    const result = await write(owner, to, name, args, 0n, {isStatic: true})
    return decodeFunctionResult({abi: abis.get(to.toLowerCase()), functionName: name, data: result.returned})
  }
  function events(result, only) {
    return (result.execResult.logs ?? []).filter(([at]) => !only || bytesToHex(at).toLowerCase() === only.toLowerCase()).map(([at, topics, data]) =>
      ({address: bytesToHex(at), ...decodeEventLog({abi: abis.get(bytesToHex(at).toLowerCase()), topics: topics.map(bytesToHex), data: bytesToHex(data)})}))
  }
  const wznn = await deploy(counterparts.WZNN), factory = await deploy(counterparts.UniswapV2Factory, [owner])
  const constructedRouter = await deploy(counterparts.UniswapV2Router02, [factory, wznn])
  const router = chainId === 7340469 ? '0xA381AE1E4C9573E38ccD95D37646C891D9e4Ca34' : chainId === 11155111 ?
    '0xC532a74256D3Db42D0Bf7a0400fEFDbad7694008' : '0x7a250d5630B4cF539739dF2C5dAcb4c659F2488D'
  // Explicit synthetic code alias: Bullet's fixed router address cannot be
  // produced by this fresh CREATE history. Copy only the runtime returned by
  // the real pinned router constructor, with its actual factory/WZNN immutables.
  const routerCode = await vm.stateManager.getCode(address(constructedRouter))
  await vm.stateManager.putAccount(address(router), createAccount({nonce: 1n}))
  await vm.stateManager.putCode(address(router), routerCode)
  assert.deepEqual(await vm.stateManager.getCode(address(router)), routerCode)
  abis.set(router.toLowerCase(), counterparts.UniswapV2Router02.abi); inventory.add(router.toLowerCase())
  assert.equal(await read(router, 'factory'), factory); assert.equal(await read(router, 'WETH'), wznn)
  const deployed = await deploy(artifacts.BulletGame, [], tokenError === undefined ? {} : {error: tokenError})
  if (tokenError !== undefined) return {failure: deployed}
  const token = deployed, escrows = []
  async function newEscrow({bettingToken = token, minimumBet = MIN, revenueBps = REVENUE, burnBps = BURN, revenueWallet = revenue} = {}) {
    const at = await deploy(artifacts.TelegramRussianRoulette, [bettingToken, minimumBet, revenueBps, burnBps, revenueWallet])
    escrows.push(at); return at
  }
  const escrow = await newEscrow()
  let pair
  async function configure() {
    for (const [name, value] of [['setMyWallet', wallet], ['setMarketingWallet', marketing], ['setRevenueWallet', revenue], ['setRouletteContract', escrow]])
      await write(owner, token, name, [value])
  }
  async function launchToken() {
    await configure(); await write(owner, token, 'enableAntiBotMode')
    const result = await write(owner, token, 'stealthLaunch', [], LIQUIDITY)
    pair = await read(token, 'pair'); abis.set(pair.toLowerCase(), counterparts.UniswapV2Pair.abi); inventory.add(pair.toLowerCase())
    assert.equal(keccak256(await vm.stateManager.getCode(address(pair))), keccak256(prefix(counterparts.UniswapV2Pair.deployedBytecode)))
    await write(owner, token, 'disableAntiBotMode')
    return result
  }
  if (launch) await launchToken()
  if (renounce) await write(owner, token, 'renounceOwnership')
  async function fundPlayers(amount = 100_000_000_000n) {
    for (const at of [alice, bob, carol, outsider]) {
      await write(marketing, token, 'transfer', [at, amount]); await write(at, token, 'connectAndApprove', [7])
    }
  }
  async function start({id = -1n, players = [alice, bob, carol], bets = [1000n, 2000n, 3000n], size = 6n,
    min = MIN, commitment = COMMITMENT, from = owner, at = escrow, ...options} = {}) {
    const result = await write(from, at, 'newGame', [id, size, min, commitment, players, bets], 0n, options)
    return {...result, bets: result.failed ? undefined : decodeFunctionResult({abi: artifacts.TelegramRussianRoulette.abi, functionName: 'newGame', data: result.returned})}
  }
  const finish = (id = -1n, loser = 0, options = {}) => write(owner, escrow, 'endGame', [id, loser, ['fixture-only']], 0n, options)
  const abort = (id = -1n, options = {}) => write(owner, escrow, 'abortGame', [id], 0n, options)
  async function reserves() {
    const [r0, r1] = await read(pair, 'getReserves'), first = await read(pair, 'token0')
    return first.toLowerCase() === token.toLowerCase() ? {tokens: r0, native: r1} : {tokens: r1, native: r0}
  }
  const buy = (from = alice, value = 10n ** 16n, options = {}) => write(from, router, 'swapExactETHForTokensSupportingFeeOnTransferTokens',
    [0n, [wznn, token], from, DEADLINE], value, options)
  async function conserved() {
    let native = 0n, bullets = 0n, wrapped = 0n
    for (const at of inventory) {
      native += await balance(at); bullets += await read(token, 'balanceOf', [at]); wrapped += await read(wznn, 'balanceOf', [at])
    }
    assert.equal(native, BigInt(actors.length) * INITIAL)
    assert.equal(bullets, await read(token, 'totalSupply'), 'all actual BULLET balances match the supply after burns')
    assert.equal(wrapped, await balance(wznn)); assert.equal(await read(wznn, 'totalSupply'), wrapped)
  }
  return {vm, actors, owner, alice, bob, carol, marketing, revenue, wallet, outsider, wznn, factory, router, constructedRouter,
    routerRuntimeHash: keccak256(routerCode), token, escrow, escrows, deployments, root, balance, frame, deploy, read, write, events,
    newEscrow, configure, launchToken, fundPlayers, start, finish, abort, reserves, buy, conserved, get pair() {return pair}}
}
