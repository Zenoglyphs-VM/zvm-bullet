# Pinned counterpart fixtures

The ABI and unchanged bytecode fields in contracts.json come from the pinned
zvm-node exports. WZNN.sol is the unchanged GPL-3.0-or-later source. The Uniswap
V2 core/periphery code is GPL-3.0; COPYING retains the full license text.
The complete retained standard JSON source inputs are in sources.json with
original unit names, comments and notices, compiler versions and build settings.
Pass each input to the stated solc version to reconstruct that compiler cohort;
this suite executes the saved exports without freshly rebuilding them.
See provenance.json for exact hashes and source attribution.
