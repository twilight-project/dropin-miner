# Chain fixtures

These seven files are **verbatim CometBFT JSON-RPC responses**, captured from the public
Twilight devnet node `http://54.179.101.3:26657` (chain `twilight-devnet-2`) on 2026-08-30 and
re-indented for review. Nothing in them was written by hand.

They are the one deliberate exception to `testdata/README.md`'s "no captured traffic" rule, and
the exception is narrow: that rule protects **provider** traffic — prompts, completions, API
keys, anything a person typed. A block chain's public RPC surface carries none of those. What
it does carry is the shape of a third-party wire protocol, and D2 §2.0's second property says
not to trust a hand-written fixture for a provider surface. Both invented `/tx_search`
responses this command was first tested against were wrong in ways only the real node showed:
the `query` parameter has to arrive as a **quoted JSON string** (an unquoted one is a `-32602`
`Invalid params`), and `total_count` is a decimal **string**, not a number.

| file | request |
| --- | --- |
| `tx_search_recipient_page1.json` | `/tx_search?query="transfer.recipient='twilight1xm9…jl6c'"&page="1"&per_page="2"&order_by="desc"` |
| `tx_search_recipient_page2.json` | the same query, `page="2"` |
| `header_161479.json`, `header_159663.json`, `header_119227.json` | `/header?height=N` for the three matched heights |
| `abci_module_account_rewards.json` | `/abci_query?path="/cosmos.auth.v1beta1.Query/ModuleAccountByName"&data=0x0a0772657761726473` |
| `abci_balance.json` | `/abci_query?path="/cosmos.bank.v1beta1.Query/Balance"` for the same address, denom `utwlt` |

The address was chosen because its three receipts are exactly the mix `earnings` has to tell
apart, and the third is the case a plausible implementation gets wrong:

- two releases from the reward escrow module account `twilight1245…c3u` (37457100 utwlt each),
  which are mining payments;
- one ordinary transfer from `twilight1kkx…mk5` (1000000 utwlt) — **inside a transaction that
  carries three `transfer` events, only one of which credits this address**. Summing a matching
  transaction's transfer events instead of filtering them by recipient reports three times the
  real amount and attributes two other participants' money to this one.

`abci_balance.json` says 75914200 utwlt, which is 37457100 × 2 + 1000000 — so the fixture set is
internally consistent and a total that disagrees with the balance is a real defect, not a
fixture artefact.

No prompt, completion, credential or provider key appears in any of these files.
