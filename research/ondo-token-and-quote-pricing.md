> ## Documentation Index
> Fetch the complete documentation index at: https://docs.ondo.finance/llms.txt
> Use this file to discover all available pages before exploring further.

# Token & Quote Pricing

<div id="geofenced-content">
  ### How is the price of an Ondo tokenized stock determined?

  Ondo tokenized stocks are designed to be *total-return trackers* of their underlying securities. This means they reflect both price movements *and* reinvested dividends and/or interest, giving users exposure to the economic performance of the real stock over time—not just the price appreciation.

  When you hold an Ondo tokenized stock, you receive similar economic exposure to what you would get if you owned the actual underlying stock and invested its dividends (net of any applicable [withholding taxes](/ondo-stocks/fees-and-taxes#how-are-ondo-tokenized-stocks-taxed)) into the stock. This ensures that the tokenized asset tracks the total return of the real-world asset.

  On Solana and BNB Chain, supported wallets and explorers use a display multiplier to adjust the token balances and prices shown to you. This allows changes in shares per token, including those resulting from dividend reinvestment, to be reflected in your displayed balance, with a corresponding adjustment to the price per displayed unit. Wallets and explorers that do not support these adjustments use a presentation similar to Ethereum, where changes in shares per token are reflected in the per-token price. These adjustments affect only how your holdings are displayed; they do not change your onchain token balance or economic exposure.

  For more on how corporate actions are handled, read more [here](/ondo-stocks/corporate-actions).

  ### Can you give me an example?

  Let's take a sample stock, ACME, and its corresponding Ondo tokenized stock, ACMEon. At launch, assume they both start at \$100 and that you buy one ACMEon token. If the ACME stock goes up by \$5 to \$105, the ACMEon stock should as well. Similarly, if the stock goes back down \$100, the same should be true for the token.

  Now let's assume—again, for a simplified example—that ACME stock declares a dividend of \$10 per share. On the ex-dividend date (i.e. the cutoff date on or after which whoever acquires the stock is no longer eligible to receive the dividend), we update the price of the token to account for the fact that we're going to receive that dividend (net of applicable withholding taxes) and invest that money to purchase additional shares of ACME.

  In this case, for ease of math let's assume the withholding tax was 50%. This means that we'd receive \$10\*(1-50%) = \$5 from the dividend. If we assume the price of ACME at the time was still \$100, that means we would buy \$5/\$100 = 0.05 additional shares of ACME stock. This means that, from now on (at least until the next dividend or corporate action), a single ACMEon token now actually represents the economics of 1.05 shares of ACME stock.  Therefore, if the price of ACME stock was to go from \$100 to \$110 again, the price of the token would go from 1.05 shares \* \$100/share = \$105 to 1.05 \* \$110/share = \$115.50.

  You can see this "shares per token" for each asset in our web app at [app.ondo.finance](https://app.ondo.finance).

  <Note>
    This means that, over time, **the price of the tokenized stock will not match the price of the underlying asset**. Instead, the price of the token will more closely reflect the same economic value that you would have if you had directly invested in the underlying stock *and* reinvested dividends back into the stock (net of any applicable withholding tax).
  </Note>

  Display scaling applies a multiplier to the displayed balance and a corresponding adjustment to the price per displayed unit. For example, with a display multiplier of 1.05, a holding shown as 1 token at \$105 would instead appear as 1.05 displayed units at \$100 each. Both presentations represent the same \$105 holding and the same economic exposure.

  | | Standard display | Adjusted display |
  | - | - | - |
  | Displayed balance | 1 token | 1.05 displayed units |
  | Price per displayed unit | \$105 | \$100 |
  | Total displayed value | \$105 | \$105 |

  The shares per token multiplier used for this conversion is published on-chain and can be viewed for each asset at app.ondo.finance.

  \[*Technical Note:* In the above example we assumed that the price of ACME stock on the ex-dividend date remained at \$100. In reality if the dividend was \$5, in most cases the price of the ACME stock would *drop* by \$5 to \$95 on the ex-dividend date. (Why? Think about it: say someone offered you a stock for \$100 today that would pay you \$5 tomorrow. If they suddenly told you that you weren't going to get the \$5 tomorrow, wouldn't you want to pay \$5 less?). So the price of the stock is now \$95. In our approach, our system effectively takes the \$5 we expect from the dividend and buys \$5/\$95 = 0.05263158 more shares, so the number of shares backing each token goes from 1 to 1.05263158, so the price of the token remains at 1.05263158 \* \$95 = \$100. In other words, the price of the token remains the same, but the number of shares represented by each token increases.]

  ### What is a total-return tracker?

  Total-return trackers are financial assets that are designed to mirror the complete performance of an index or asset. Unlike *price* return trackers, which only capture an asset's market value changes, *total* return trackers account for all cash flows—dividends, interest, or capital gains distributions—assuming these are reinvested to compound returns (net of applicable withholding taxes).

  A total-return tracker includes:

  * Price movements – gains or losses in the asset's market value
  * Income distributions – such as dividends (for stocks) or interest (for bonds)
  * Corporate actions – like mergers that may affect total value ([learn more](/ondo-stocks/corporate-actions))

  By capturing both capital gains and income, total-return trackers offer a more comprehensive reflection of an asset's performance than price-only trackers.

  ### When I am actually buying or selling an Ondo tokenized stock, the price seems to be slightly different from the main price listed? Why?

  Answering this requires a little bit more explanation about how our Ondo Stocks platform works.

  Let's say you're trying to buy Ondo tokenized Tesla, or TSLAon. What you really want to know is "at what price will you (the Ondo Stocks platform) sell me (the investor) X number of TSLAon tokens?" As you might imagine, the answer to that is tied to a very similar question: "at what price will someone (in the traditional market) sell *us* X number of TSLA *shares*?"

  So when you indicate that you want to buy X number of TSLAon tokens, we provide you a quote for a price that's guaranteed for some amount of time (currently about 30 seconds). To generate that quote, the Ondo Stocks platform automatically looks at existing inventory, market conditions, and other factors and then does a little bit of (proprietary) math to figure out what an appropriate quote price should be for that order.

  For Ondo tokenized stocks, the "main" price displayed is an indicative token price calculated using our proprietary pricing methodology based on prevailing market conditions. It accounts for the number of [shares represented by each token](/ondo-stocks/token-and-quote-pricing#can-you-give-me-an-example). The "quote price" is calculated for your specific buy or sell order, taking into account the order size, available inventory, market conditions, and other factors. It is the price at which the Ondo Stocks platform offers to buy or sell the specified number of tokens during the quote's validity period, and may differ from the displayed indicative price.

  (Please note that if you are buying an Ondo tokenized stock on the secondary market, whomever you are transacting with may be charging their own fees. You are also responsible for your own gas costs.)

  ### How are prices determined during the Off-Hours session (weekends and holidays)?

  During the Off-Hours session, when the traditional US markets for the underlying securities are closed, quotes for the assets enabled for Off-Hours trading are generated using the same proprietary pricing method, based on prevailing market conditions. Because liquidity is generally lower while the traditional markets are closed, bid-ask spreads can be wider than during an active session, available size is subject to conservative per-asset Off-Hours limits, and the price of a token may diverge more significantly from the value the underlying security will have when its primary market next reopens. See [Off-Hours Trading](/ondo-stocks/off-hours-trading).

  ### How do you determine the spread to apply on a quote?

  This is based upon a number of factors, including quote size, target profit, and several other proprietary considerations.
</div>


This documentation is built and hosted on [Mintlify](https://mintlify.com), a developer documentation platform.
