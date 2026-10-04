# Frequently Asked Questions

details
summary
What are xStocks?
xStocks are permissionless tokenized representations of publicly traded stocks and ETFs.

Each xStock provides economic exposure to a specific underlying equity and is fully collateralized 1:1 by that asset. The underlying securities are held with regulated custodians under a bankruptcy-remote structure.

details
summary
What do I own when I hold an xStock?
An xStock is a tracker certificate. They provide economic exposure to the underlying equity but do not convey shareholder rights, such as voting rights. Ownership is recorded on a public blockchain.

details
summary
Are xStocks fully backed?
Yes.

Each xStock is collateralized on a 1:1 basis. The corresponding underlying securites are held in segregated custody accounts. [Proof of reserves](https://defi.xstocks.fi/proof-of-reserves) is publicly available.

details
summary
Are xStocks freely transferable?
Yes.

xStocks are issued using the SPL Token-2022 standard (Solana) or ERC-20 standard (EVM-compatible chains). They can be transferred onchain like other tokens.

details
summary
Where can I buy xStocks?
xStocks are available through supported exchanges and platforms. A list of official partners is available at [xstocks.fi](http://xstocks.fi/).

Eligible direct clients may also issue or redeem xStocks directly through the issuer’s platform.

details
summary
How do dividends work?
Dividends received on the underlying equity are reinvested into additional units of the same asset.

This is reflected in the token through a rebasing mechanism:

* On EVM chains, balances adjust automatically.
* On Solana, adjusted balances are displayed using Scaled UI.


Dividends are reinvested net of applicable taxes.

details
summary
How do stock splits and reverse splits work?
Stock splits and reverse splits are handled through the same rebasing mechanism.

If a stock undergoes a split, token balances adjust proportionally to reflect the change. No action is required from holders.

details
summary
How do xStocks operate during market off-hours?
xStocks are issued as tokens following SPL Token-2022 or ERC-20 token standards, and can be traded on secondary markets 24/7 like any other token. Issuance and redemption by minters and redeemers are limited to business days when the US market is open, normally 24/5.

details
summary
What is rebasing?
Rebasing is the mechanism used to reflect corporate actions such as dividends, stock splits, and reverse splits.

When a corporate action occurs, a multiplier is applied to token balances so that they always reflect a 1:1 exposure of the underlying equity. No action is required from holders.

The multiplier is implemented differently depending on the blockchain. On EVM chains, the token contract adjusts balances automatically. On Solana, the raw onchain balance remains constant and the multiplier is applied for display using the Scaled UI extension.

details
summary
Can xStocks trade outside of traditional market hours?
Yes.

xStocks can be traded 24/7 on secondary markets, depending on the platform.

Issuance and redemption through the issuer operates 24/5, aligned with underlying market hours.

details
summary
How is the price of xStocks determined?
In the secondary market, pricing is determined by supply and demand on each platform.

Direct clients may issue or redeem xStocks at prevailing market prices of the underlying equity through the issuer’s platform.

The issuer does not control pricing on secondary markets.

details
summary
Is there a minimum transaction size?
On secondary markets, there is no minimum imposed by the issuer. xStocks can be traded like other tokens, subject to the rules of the platform where they are listed.

If interacting directly with the issuer for issuance or redemption, the minimum transaction size is $5,000.

details
summary
Can retail users redeem directly with the issuer?
Yes.

Retail users are legally permitted to redeem directly with the issuer, subject to KYC requirements and the $5,000 minimum transaction size.

In practice, most users access liquidity through secondary markets.

details
summary
Are xStocks available in the United States?
xStocks are not marketed, offered, or solicited in the United States or in jurisdictions where such activity is prohibited.

details
summary
Is there proof of reserves?
Yes.

Collateral backing is publicly verifiable. Proof of reserves information is available through the [xStocks DeFi portal](http://defi.xstocks.fi/).

details
summary
On which blockchains is xStocks available?
xStocks are issued natively on Ethereum, Solana, Arbitrum, Mantle, TON, Ink, and other EVM-compatible networks.

[The xStocks CCIP bridge](https://defi.xstocks.fi/bridge) allows movement between supported networks without selling and buying the asset.

details
summary
What happens if the issuer defaults?
Collateral is held in segregated accounts under a three-party structure involving the issuer, custodians, and an independent security agent.

In the event of issuer default, the security agent may assume control of the collateral accounts and distribute proceeds to token holders in accordance with the prospectus terms. For the complete legal documentation please visit the [issuer site](https://assets.backed.fi/).
