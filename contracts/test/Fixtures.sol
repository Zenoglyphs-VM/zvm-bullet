// SPDX-License-Identifier: MIT
pragma solidity 0.8.19;

// Only used to force late escrow failures. Normal games use real BulletGame.
contract FailingToken {
    mapping(address => uint256) public balanceOf;
    mapping(address => mapping(address => uint256)) public allowance;
    uint256 public totalSupply;
    uint256 public calls;
    uint256 public failAt;
    function mint(address to, uint256 amount) external { balanceOf[to] += amount; totalSupply += amount; }
    function approve(address to, uint256 amount) external returns (bool) { allowance[msg.sender][to] = amount; return true; }
    function configure(uint256 at) external { calls = 0; failAt = at; }
    function transferFrom(address from, address to, uint256 amount) external returns (bool) {
        allowance[from][msg.sender] -= amount; return move(from, to, amount);
    }
    function transfer(address to, uint256 amount) external returns (bool) { return move(msg.sender, to, amount); }
    function move(address from, address to, uint256 amount) private returns (bool) {
        balanceOf[from] -= amount; balanceOf[to] += amount; calls++;
        return failAt == 0 || calls != failAt;
    }
    function burn(uint256 amount) external { balanceOf[msg.sender] -= amount; totalSupply -= amount; }
}
