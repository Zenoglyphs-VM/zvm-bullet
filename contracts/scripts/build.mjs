// Builds what the ZVM runs into ../internal/contracts/artifacts, where the Go
// deploy command embeds it:
//
//   BulletGame               zvm/BulletGame.sol, mainnet's token plus one
//                            constructor branch naming the ZVM's router
//   TelegramRussianRoulette  mainnet/TelegramRussianRoulette.sol, unchanged
//
// Each artifact carries the standard JSON input that produced it, so the
// node's verifier is handed exactly what was compiled.
//
// The escrow is checked against mainnet's executable code on every build: if a
// future edit touched it, this fails rather than quietly deploying something
// that is no longer the contract the game was audited as.
import {mkdirSync, readFileSync, writeFileSync} from 'node:fs'
import {compile, stripMetadata, version} from './solc.mjs'

const out = new URL('../../internal/contracts/artifacts/', import.meta.url)
mkdirSync(out, {recursive: true})
const mainnet = JSON.parse(readFileSync(new URL('../mainnet/mainnet.json', import.meta.url), 'utf8'))

const units = [
  {name: 'BulletGame', unit: 'BulletGame.sol', path: '../zvm/BulletGame.sol'},
  {name: 'TelegramRussianRoulette', unit: 'TelegramRussianRoulette.sol', path: '../mainnet/TelegramRussianRoulette.sol', sameAsMainnet: true},
]

for (const u of units) {
  const {input, output} = compile(u.unit, new URL(u.path, import.meta.url), u.name)
  const bytecode = '0x' + output.evm.bytecode.object
  if (u.sameAsMainnet) {
    const m = mainnet[u.name]
    const onchain = m.creationBytecode.toLowerCase().replace(/^0x/, '')
    const code = onchain.slice(0, onchain.length - (m.constructorArgs || '').length)
    if (stripMetadata(bytecode) !== stripMetadata(code)) throw new Error(`${u.name} no longer matches mainnet's executable code`)
  }
  const artifact = {
    contractName: u.name,
    sourceName: u.unit,
    compiler: version(),
    compilerVersion: 'v0.8.19+commit.7dd6d404',
    abi: output.abi,
    bytecode,
    deployedBytecode: '0x' + output.evm.deployedBytecode.object,
    standardJsonInput: input,
  }
  // The standard JSON input carries outputSelection for this build; the
  // verifier only needs the settings that change bytecode, but passing it
  // whole is harmless and keeps it byte for byte what was compiled.
  writeFileSync(new URL(`${u.name}.json`, out), JSON.stringify(artifact, null, 1) + '\n')
  console.log(`${u.name}: ${(bytecode.length - 2) / 2} bytes creation code${u.sameAsMainnet ? ', executable code identical to mainnet' : ''}`)
}
