// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {ERC20} from "@openzeppelin/contracts/token/ERC20/ERC20.sol";

/// @title Mock USDT (mUSDT)
/// @notice The ShopHub project's test token for Ethereum Sepolia (spec 005, constitution
/// Principle IV). It has no value: anyone may mint up to 1,000 mUSDT per call to try the shops.
/// No owner, no pause, no blacklist.
contract MockUSDT is ERC20 {
    /// @notice The most one mint call may create: 1,000 mUSDT with 6 decimals.
    uint256 public constant MAX_MINT = 1_000 * 10 ** 6;

    /// @notice A mint asked for more than MAX_MINT.
    error MintTooLarge(uint256 requested, uint256 max);

    constructor() ERC20("Mock USDT", "mUSDT") {}

    /// @notice 6 decimals, like USDT.
    function decimals() public pure override returns (uint8) {
        return 6;
    }

    /// @notice Mints `amount` (base units) to the caller.
    function mint(uint256 amount) external {
        if (amount > MAX_MINT) {
            revert MintTooLarge(amount, MAX_MINT);
        }
        _mint(msg.sender, amount);
    }
}
