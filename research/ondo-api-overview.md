> ## Documentation Index
> Fetch the complete documentation index at: https://docs.ondo.finance/llms.txt
> Use this file to discover all available pages before exploring further.

# Overview

> An overview of the Ondo API and smart contract layer for Ondo Stocks and Ondo Intelligent Portfolios.

The Ondo API provides programmatic access to two onchain product lines: Ondo Stocks—tokenized publicly-traded US stocks and ETFs—and Ondo Intelligent Portfolios—curated investment portfolios delivered as single onchain transferable tokens.

Ondo Stocks tokenizes publicly-traded US stocks and ETFs, allowing investors outside the United States to mint, transfer, and redeem equity-backed tokens across multiple public blockchains. Ondo Stocks are available on Ethereum, BNB Chain, and Solana. On-chain assets are issued as ERC-20 tokens for EVM-based chains and as SPL Token-2022 tokens on Solana. Each token represents direct exposure to the underlying asset, held with a regulated custodian through a licensed broker-dealer.

Ondo Intelligent Portfolios builds on the foundation of Ondo Stocks: professionally constructed portfolios delivered as single onchain tokens. Each portfolio token provides economic exposure to a weighted basket of Ondo Stocks tokens managed to a defined allocation. Investors mint or redeem a single token to hold the full basket, without buying, weighting, or rebalancing individual positions themselves. Rebalancing back to base weights executes programmatically, and holdings, weights, and every rebalance are visible onchain. Ondo Intelligent Portfolios are available on Ethereum and BNB Chain and issued as ERC-20 tokens for EVM-based chains.

To support integration across the ecosystem, we offer a suite of APIs designed for infrastructure providers such as wallets, exchanges, and fintech applications covering both product lines.

<Info>
  **Terminology:** Throughout the REST and streaming APIs and onchain identifiers, Ondo Stocks & Intelligent Portfolios use the legacy `GM` prefix—for example, the symbol parameter is described as the "GM token symbol" and the REST spec is titled the "GM Backend API". The smart contract manager depends on the product line: Ondo Stocks use `IGMTokenManager`, while Ondo Intelligent Portfolios use `PortfolioTokenManager`.
</Info>

## API Surfaces

The API is delivered through two surfaces, alongside the on-chain smart contract layer. The REST API spec can be found at [docs.ondo.finance/openapi.json](https://docs.ondo.finance/openapi.json), and the streaming API's [Protocol Buffers schema](/api-reference/protobuf-schema) is available for generating typed clients.

<CardGroup cols={2}>
  <Card title="REST API" icon="server" href="https://docs.ondo.finance/openapi.json">
    High-performance request/response access to essential pricing, quote generation, token lifecycle, and token contract metadata. The full OpenAPI specification is available for use with code generators and other tooling.
  </Card>

  <Card title="Streaming API" icon="bolt" href="/api-reference/protobuf-schema">
    Low-latency, real-time updates over gRPC so clients can react to market movements as they happen rather than polling. The Protocol Buffers schema is available for generating typed clients.
  </Card>
</CardGroup>

## What the API Supports

* [Generating mint and redemption attestations](/api-reference/attestations/request-a-mint-or-redeem-attestation) to generate quotes that are fed to the [smart contract layer](/api-reference/smart-contracts).
* [Real-time price data](/api-reference/assets/get-current-prices-for-all-supported-assets) for Ondo Stocks & Intelligent Portfolios assets.
* [OHLC (Open, High, Low, Close) data](/api-reference/assets/get-ohlc-open-high-low-close-data-for-an-asset) to support charting, analytics, and historical views.
* [On chain mints and redemptions](/api-reference/smart-contracts) that handle the core logic for minting and redeeming Ondo Stocks & Intelligent Portfolios.

The [smart contract layer](/api-reference/smart-contracts) provides the ability to mint and redeem (buy/sell) the tokenized asset.

## How It Works

Here's the typical flow for Ondo Stocks & Intelligent Portfolios, using the Ondo Stocks token TSLAon as an example:

**Minting Process:**

1. Primary Purchaser requests a mint attestation from the Ondo Stocks & Intelligent Portfolios API.
2. Primary Purchaser broadcasts transaction with the provided mint attestation signature along with a USD stablecoin (such as USDC) to the smart contract to atomically mint TSLAon to their whitelisted address.

**Redeeming Process:**

1. Primary Purchaser requests a redemption attestation from the Ondo Stocks & Intelligent Portfolios API.
2. Primary Purchaser broadcasts transaction with the provided redemption attestation signature along with TSLAon to the smart contract to atomically redeem TSLAon for a USD stablecoin (such as USDC) to their whitelisted address.

## Get Access

If you're interested in getting access to the Ondo Stocks & Intelligent Portfolios API, reach out to [onboarding@ondo.finance](mailto:onboarding@ondo.finance).


This documentation is built and hosted on [Mintlify](https://mintlify.com), a developer documentation platform.
